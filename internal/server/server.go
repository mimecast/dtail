package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	user "github.com/mimecast/dtail/internal/sessionuser"
	"github.com/mimecast/dtail/internal/ssh/server"
	"github.com/mimecast/dtail/internal/version"

	gossh "golang.org/x/crypto/ssh"
)

const sshHandshakeTimeout = 10 * time.Second

// Server is the main server data structure.
type Server struct {
	cfg          config.RuntimeConfig
	logger       logging.Logger
	readerLogger logging.Logger
	colorizer    *brush.Brush
	hostname     string
	// Various server statistics counters.
	stats stats
	// SSH server configuration.
	sshServerConfig *gossh.ServerConfig
	// To control the max amount of concurrent cats.
	catLimiter chan struct{}
	// To control the max amount of concurrent tails.
	tailLimiter chan struct{}
	// Background jobs are composed by cmd/dserver so this package does not depend on clients.
	backgroundJobs BackgroundJobs
	// Capabilities are detected once during construction and passed to every session handler.
	capabilities []string
	// Authentication strategies keyed by SSH username.
	authStrategies map[string]authStrategy
	// In-memory auth key cache for fast reconnect.
	authKeyStore *authkey.Store
	listen       func(context.Context, string, string) (net.Listener, error)
	lookupIPAddr func(context.Context, string) ([]net.IPAddr, error)
}

type authStrategy func(context.Context, *user.User, string, string) bool

// BackgroundJobs runs configured scheduled and continuous client workloads.
type BackgroundJobs interface {
	Start(context.Context)
}

func (s *Server) log() logging.Logger {
	if s.logger == nil {
		return logging.NopLogger{}
	}
	return s.logger
}

func (s *Server) readerLog() logging.Logger {
	if s.readerLogger == nil {
		return s.log()
	}
	return s.readerLogger
}

// secretsEqual compares two secret strings in constant time to prevent
// timing side-channel attacks. Unlike plain ==, this function takes the same
// amount of time regardless of where the first differing byte is, so an
// attacker who can measure response latency cannot recover the secret.
func secretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// New returns a new server.
func New(cfg config.RuntimeConfig, loggers handlers.HandlerLoggers, backgroundJobs BackgroundJobs,
	colorizers ...*brush.Brush) (*Server, error) {
	var colorizer *brush.Brush
	if len(colorizers) > 0 {
		colorizer = colorizers[0]
	}
	logger := logging.OrNop(loggers.Diagnostics)
	if cfg.Server == nil || cfg.Common == nil {
		if fatalLogger, ok := logger.(interface{ FatalPanic(...any) }); ok {
			fatalLogger.FatalPanic("Missing runtime server/common configuration")
		}
		panic("Missing runtime server/common configuration")
	}

	logger.Info("Starting server", version.String())
	hostname, err := cfg.Hostname()
	if err != nil {
		return nil, fmt.Errorf("resolve server hostname: %w", err)
	}

	s := Server{
		cfg:          cfg,
		logger:       logger,
		readerLogger: logging.OrNop(loggers.Reader),
		colorizer:    colorizer,
		hostname:     hostname,
		sshServerConfig: &gossh.ServerConfig{
			Config: gossh.Config{
				KeyExchanges: cfg.Server.KeyExchanges,
				Ciphers:      cfg.Server.Ciphers,
				MACs:         cfg.Server.MACs,
			},
		},
		stats:          newStats(cfg.Server.MaxConnections, logger),
		catLimiter:     make(chan struct{}, cfg.Server.MaxConcurrentCats),
		tailLimiter:    make(chan struct{}, cfg.Server.MaxConcurrentTails),
		backgroundJobs: backgroundJobs,
		capabilities:   handlers.DetectCapabilities(),
		authKeyStore: authkey.New(
			time.Duration(cfg.Server.AuthKeyTTLSeconds)*time.Second,
			cfg.Server.AuthKeyMaxPerUser,
		),
	}
	s.authStrategies = s.newAuthStrategies()

	s.sshServerConfig.PublicKeyCallback = server.NewPublicKeyCallback(
		cfg.Server.AuthKeyEnabled,
		cfg.Common.CacheDir,
		cfg.Server.AuthorizedKeysPath,
		s.authKeyStore,
		logger,
	)

	privateKey, err := server.PrivateHostKey(cfg.Server.EffectiveHostKeyPath(), cfg.Server.HostKeyBits, logger)
	if err != nil {
		return nil, fmt.Errorf("load SSH host key: %w", err)
	}
	private, err := gossh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse SSH host key: %w", err)
	}
	s.sshServerConfig.AddHostKey(private)

	return &s, nil
}

// Start the server.
func (s *Server) Start(ctx context.Context) (int, error) {
	if ctx == nil {
		return 1, fmt.Errorf("start server: context must not be nil")
	}
	s.log().Info("Starting server")
	bindAt := net.JoinHostPort(s.cfg.Server.SSHBindAddress, fmt.Sprintf("%d", s.cfg.Common.SSHPort))
	s.log().Info("Binding server", bindAt)

	listener, err := s.listenContext(ctx, "tcp", bindAt)
	if err != nil {
		return 1, fmt.Errorf("listen on %s: %w", bindAt, err)
	}

	go s.stats.start(ctx)
	if s.backgroundJobs != nil {
		go s.backgroundJobs.Start(ctx)
	}
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		s.listenerLoop(ctx, listener)
	}()

	<-ctx.Done()
	if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		s.log().Trace("Close listener", closeErr)
	}
	<-listenerDone
	// For future use.
	return 0, nil
}

func (s *Server) listenContext(ctx context.Context, network, address string) (net.Listener, error) {
	if s.listen != nil {
		return s.listen(ctx, network, address)
	}
	var listenConfig net.ListenConfig
	return listenConfig.Listen(ctx, network, address)
}

func (s *Server) listenerLoop(ctx context.Context, listener net.Listener) {
	s.log().Debug("Starting listener loop")
	for {
		conn, err := listener.Accept() // Blocking
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.log().Error("Failed to accept incoming connection", err)
			continue
		}
		if ctx.Err() != nil {
			_ = conn.Close()
			return
		}

		if limitErr := s.stats.serverLimitExceeded(); limitErr != nil {
			s.log().Error(limitErr)
			_ = conn.Close()
			continue
		}
		// Reserve a pre-auth slot immediately after the limit check so that
		// in-progress handshakes count against maxConnections. The slot is
		// released inside handleConnection — either promoted to a full
		// connection on success, or freed on any failure path.
		s.stats.reservePreAuth()
		go s.handleConnection(ctx, conn)
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	connCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	stopCloseOnCancel := context.AfterFunc(connCtx, func() { _ = conn.Close() })
	defer stopCloseOnCancel()
	defer s.recoverGoroutinePanic("SSH connection", conn.RemoteAddr(), nil)
	defer func() { _ = conn.Close() }()

	s.log().Info("Handling connection")

	// The caller (listenerLoop) already reserved a pre-auth slot via
	// reservePreAuth. We must release it on every early-exit path. On the
	// happy path we promote it atomically to a full connection instead.
	preAuthReleased := false
	releasePreAuth := func() {
		if !preAuthReleased {
			s.stats.releasePreAuth()
			preAuthReleased = true
		}
	}
	defer releasePreAuth()

	// Prevent slow clients from holding connections open indefinitely before SSH handshake completes.
	if deadlineErr := conn.SetDeadline(time.Now().Add(sshHandshakeTimeout)); deadlineErr != nil {
		s.log().Error("Failed to set SSH handshake deadline", deadlineErr)
		_ = conn.Close()
		releasePreAuth()
		return
	}

	activeConn := newActivityConn(conn)
	sshConfig := *s.sshServerConfig
	sshConfig.PasswordCallback = func(metadata gossh.ConnMetadata, authPayload []byte) (*gossh.Permissions, error) {
		return s.Callback(connCtx, metadata, authPayload)
	}
	sshConn, chans, reqs, err := gossh.NewServerConn(activeConn, &sshConfig)
	if err != nil {
		// Handshake failed (auth error, timeout, or connection reset).
		// Release the pre-auth slot so the limit accurately reflects reality.
		s.log().Error("SSH handshake failed", err)
		releasePreAuth()
		return
	}

	// Handshake succeeded. Replace the fixed handshake deadline with a rolling
	// inactivity deadline that a per-connection ticker refreshes after activity.
	idleTimeout := time.Duration(s.cfg.Server.IdleSessionTimeoutS) * time.Second
	if idleTimeout <= 0 {
		idleTimeout = time.Duration(config.DefaultIdleSessionTimeoutS) * time.Second
	}
	if deadlineErr := activeConn.enable(connCtx, idleTimeout); deadlineErr != nil {
		s.log().Error("Failed to set SSH idle session deadline", deadlineErr)
		_ = sshConn.Close()
		return
	}
	// Stop the idle deadline refresher and wait for it before returning.
	defer func() { _ = activeConn.Close() }()

	// Atomically convert the pre-auth reservation into a full authenticated
	// connection. This ensures no instant where neither counter holds the slot,
	// keeping the effective connection count consistent with maxConnections.
	s.stats.promotePreAuthToConnection()
	preAuthReleased = true // pre-auth slot was consumed by promote; guard is no longer needed
	defer s.stats.decrementConnections()

	go func() {
		defer s.recoverGoroutinePanic("SSH global request", sshConn.RemoteAddr(), func() { _ = sshConn.Close() })
		gossh.DiscardRequests(reqs)
	}()
	for newChannel := range chans {
		go s.handleChannel(connCtx, sshConn, newChannel)
	}
}

func (s *Server) handleChannel(ctx context.Context, sshConn gossh.Conn,
	newChannel gossh.NewChannel) {
	defer s.recoverGoroutinePanic("SSH channel", sshConn.RemoteAddr(), func() { _ = sshConn.Close() })

	serverUser, userErr := user.New(sshConn.User(), sshConn.RemoteAddr().String(), s.cfg.Server.UserPermissions, s.log())
	if userErr != nil {
		s.log().Error(serverUser, userErr)
		if rejectErr := newChannel.Reject(gossh.Prohibited, userErr.Error()); rejectErr != nil {
			s.log().Debug(rejectErr)
		}
		return
	}

	s.log().Info(serverUser, "Invoking channel handler")
	if newChannel.ChannelType() != "session" {
		channelTypeErr := errors.New("don't allow channel types other than session")
		s.log().Error(serverUser, channelTypeErr)
		if rejectErr := newChannel.Reject(gossh.Prohibited, channelTypeErr.Error()); rejectErr != nil {
			s.log().Debug(rejectErr)
		}
		return
	}

	channel, requests, acceptErr := newChannel.Accept()
	if acceptErr != nil {
		s.log().Error(serverUser, "Could not accept channel", acceptErr)
		return
	}

	if err := s.handleRequests(ctx, sshConn, requests, channel, serverUser); err != nil {
		s.log().Error(serverUser, err)
		_ = sshConn.Close()
	}
}

func (s *Server) handleRequests(ctx context.Context, sshConn gossh.Conn,
	in <-chan *gossh.Request, channel gossh.Channel, user *user.User) error {

	s.log().Info(user, "Invoking request handler")
	for req := range in {
		var payload = struct{ Value string }{}
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			s.log().Error(user, err)
		}

		switch req.Type {
		case "shell":
			s.handleShellRequest(ctx, sshConn, channel, user, req)
		default:
			if err := req.Reply(false, nil); err != nil {
				s.log().Trace(user, fmt.Errorf("reply(false): %w", err))
			}
			return fmt.Errorf("closing SSH connection as unknown request received|%s|%v",
				req.Type, payload.Value)
		}
	}
	return nil
}

// handleShellRequest sets up the shell session with handler goroutines for I/O,
// context cancellation, and connection lifecycle management.
func (s *Server) handleShellRequest(ctx context.Context, sshConn gossh.Conn,
	channel gossh.Channel, user *user.User, req *gossh.Request) {

	handler, err := handlers.NewForUser(ctx, user, handlers.Dependencies{
		ServerConfig: s.cfg.Server,
		CatLimiter:   s.catLimiter,
		TailLimiter:  s.tailLimiter,
		AuthKeyStore: s.authKeyStore,
		Loggers: handlers.HandlerLoggers{
			Diagnostics: s.log(),
			Reader:      s.readerLog(),
		},
		Capabilities: s.capabilities,
		Colorizer:    s.colorizer,
		Hostname:     s.hostname,
	})
	if err != nil {
		s.log().Error(user, "Unable to create session handler", err)
		if replyErr := req.Reply(false, nil); replyErr != nil {
			s.log().Trace(user, fmt.Errorf("reply(false): %w", replyErr))
		}
		if closeErr := sshConn.Close(); closeErr != nil {
			s.log().Trace(user, fmt.Errorf("close failed session connection: %w", closeErr))
		}
		return
	}

	// Publish the transport reader before either I/O goroutine can dispatch
	// commands. Input and output goroutines start independently; without this
	// attachment signal, a fast command can reach shutdown before Read starts
	// and incorrectly look like a serverless/raw-channel consumer.
	if outputReader, ok := handler.(interface{ AttachOutputReader() }); ok {
		outputReader.AttachOutputReader()
	}

	var terminateOnce sync.Once
	terminate := func() {
		defer s.recoverGoroutinePanic("session termination", user, func() { _ = sshConn.Close() })
		terminateOnce.Do(func() {
			defer func() {
				if closeErr := sshConn.Close(); closeErr != nil {
					s.log().Trace(user, fmt.Errorf("close session connection: %w", closeErr))
				}
			}()
			handler.Shutdown()
		})
	}

	// Start goroutine to copy data from channel to handler
	go func() {
		defer terminate()
		defer s.recoverGoroutinePanic("session input", user, func() { _ = sshConn.Close() })
		if _, copyErr := io.Copy(channel, handler); copyErr != nil {
			s.log().Trace(user, fmt.Errorf("channel->handler: %w", copyErr))
		}
	}()

	// Start goroutine to copy data from handler to channel
	go func() {
		defer terminate()
		defer s.recoverGoroutinePanic("session output", user, func() { _ = sshConn.Close() })
		if _, copyErr := io.Copy(handler, channel); copyErr != nil {
			s.log().Trace(user, fmt.Errorf("handler->channel: %w", copyErr))
		}
	}()

	// Start goroutine to handle context or handler completion
	go func() {
		defer terminate()
		defer s.recoverGoroutinePanic("session lifecycle", user, func() { _ = sshConn.Close() })
		select {
		case <-ctx.Done():
		case <-handler.Done():
		}
	}()

	// Start goroutine to handle connection lifecycle and cleanup.
	// Note: connection-counter management (increment/decrement) is done in
	// handleConnection via defer, not here, so that the counter is balanced
	// 1:1 per TCP connection regardless of how many shell requests are opened.
	go func() {
		defer terminate()
		defer s.recoverGoroutinePanic("session connection wait", user, func() { _ = sshConn.Close() })
		if waitErr := sshConn.Wait(); waitErr != nil && !errors.Is(waitErr, io.EOF) {
			s.log().Error(user, waitErr)
		}
		s.log().Info(user, "Good bye Mister!")
	}()

	// Reply to indicate shell request was accepted
	if replyErr := req.Reply(true, nil); replyErr != nil {
		s.log().Trace(user, fmt.Errorf("reply(true): %w", replyErr))
	}
}

// Callback for SSH authentication.
func (s *Server) Callback(ctx context.Context, c gossh.ConnMetadata,
	authPayload []byte) (*gossh.Permissions, error) {
	if ctx == nil {
		return nil, fmt.Errorf("authenticate SSH connection: context must not be nil")
	}

	authenticatedUser, err := user.New(c.User(), c.RemoteAddr().String(), s.cfg.Server.UserPermissions, s.log())
	if err != nil {
		return nil, err
	}

	authInfo := string(authPayload)
	remoteAddr := c.RemoteAddr().String()
	remoteIP, _, splitErr := net.SplitHostPort(remoteAddr)
	if splitErr != nil {
		s.log().Debug(authenticatedUser, "Unable to split remote address host/port, using raw address",
			"remoteAddr", remoteAddr, "error", splitErr)
		remoteIP = remoteAddr
	}

	if strategy, found := s.authStrategies[authenticatedUser.Name]; found && strategy(ctx, authenticatedUser, authInfo, remoteIP) {
		return nil, nil
	}

	return nil, fmt.Errorf("user %s not authorized", authenticatedUser)
}

func (s *Server) newAuthStrategies() map[string]authStrategy {
	return map[string]authStrategy{
		config.HealthUser:     s.authorizeHealthUser,
		config.ScheduleUser:   s.authorizeScheduleUser,
		config.ContinuousUser: s.authorizeContinuousUser,
	}
}

func (s *Server) authorizeHealthUser(ctx context.Context, user *user.User, authInfo, _ string) bool {
	if ctx.Err() != nil {
		return false
	}
	// Use constant-time comparison to avoid timing side-channel attacks.
	// An attacker who can measure response latency must not be able to infer
	// how many bytes of the secret matched.
	if !secretsEqual(authInfo, config.HealthUser) {
		return false
	}
	s.log().Debug(user, "Granting permissions to health user")
	return true
}

func (s *Server) authorizeScheduleUser(ctx context.Context, user *user.User, authInfo, remoteIP string) bool {
	for i := range s.cfg.Server.Schedule {
		if ctx.Err() != nil {
			return false
		}
		job := &s.cfg.Server.Schedule[i]
		if s.backgroundCanSSH(ctx, user, authInfo, remoteIP, job.Name, job.AllowFrom) {
			s.log().Debug(user, "Granting SSH connection")
			return true
		}
	}
	return false
}

func (s *Server) authorizeContinuousUser(ctx context.Context, user *user.User, authInfo, remoteIP string) bool {
	for i := range s.cfg.Server.Continuous {
		if ctx.Err() != nil {
			return false
		}
		job := &s.cfg.Server.Continuous[i]
		if s.backgroundCanSSH(ctx, user, authInfo, remoteIP, job.Name, job.AllowFrom) {
			s.log().Debug(user, "Granting SSH connection")
			return true
		}
	}
	return false
}

// backgroundCanSSH checks whether a background SSH connection is authorised.
// The caller passes authInfo (the client-presented password/secret) and
// allowedJobName (the operator-configured value from the server config).
//
// Security notes:
//   - The secret comparison MUST use secretsEqual (crypto/subtle) to prevent
//     timing side-channel attacks; do NOT revert to plain ==.
//   - authInfo/jobName MUST NOT appear in any log line — if debug logging is
//     ever enabled in production the shared secret would leak. Log only the
//     operator-visible allowedJobName or a fixed placeholder.
func (s *Server) backgroundCanSSH(ctx context.Context, user *user.User, authInfo, remoteIP,
	allowedJobName string, allowFrom []string) bool {

	// Do not log authInfo (the client-presented secret) — only log the
	// operator-configured job name so the shared secret cannot leak into
	// debug logs.
	s.log().Debug("backgroundCanSSH", user, remoteIP, "allowedJobName", allowedJobName, allowFrom)

	// Constant-time comparison prevents a remote attacker from recovering the
	// secret by measuring how long the server takes to reject wrong values.
	if !secretsEqual(authInfo, allowedJobName) {
		s.log().Debug(user, "backgroundCanSSH",
			"Job name does not match, skipping to next one...", "allowedJobName", allowedJobName)
		return false
	}
	if ctx.Err() != nil {
		return false
	}

	for _, myAddr := range allowFrom {
		ips, err := s.lookupIPAddresses(ctx, myAddr)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			s.log().Debug(user, "backgroundCanSSH", "Unable to lookup IP "+
				"address for allowed hosts lookup, skipping to next one...",
				"allowedJobName", allowedJobName, "addr", myAddr, "error", err)
			continue
		}
		for _, ip := range ips {
			s.log().Debug(user, "backgroundCanSSH", "Comparing IP addresses",
				"allowedJobName", allowedJobName, "remoteIP", remoteIP, "candidateIP", ip.IP.String())
			if remoteIP == ip.IP.String() {
				return true
			}
		}
	}

	return false
}

func (s *Server) lookupIPAddresses(ctx context.Context, host string) ([]net.IPAddr, error) {
	if s.lookupIPAddr != nil {
		return s.lookupIPAddr(ctx, host)
	}
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}
