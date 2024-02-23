package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/server/handlers"
	"github.com/mimecast/dtail/internal/ssh/server"
	user "github.com/mimecast/dtail/internal/user/server"
	"github.com/mimecast/dtail/internal/version"

	gossh "golang.org/x/crypto/ssh"
)

// Server is the main server data structure.
type Server struct {
	// Various server statistics counters.
	stats stats
	// SSH server configuration.
	sshServerConfig *gossh.ServerConfig
	// To control the max amount of concurrent cats.
	catLimiter chan struct{}
	// To control the max amount of concurrent tails.
	tailLimiter chan struct{}
	// To run scheduled tasks (if configured)
	sched *scheduler
	// Mointor log files for pattern (if configured)
	cont *continuous
}

// New returns a new server.
func New() *Server {
	dlog.Server.Info("Starting server", version.String())

	s := Server{
		sshServerConfig: &gossh.ServerConfig{
			Config: gossh.Config{
				KeyExchanges: config.Server.KeyExchanges,
				Ciphers:      config.Server.Ciphers,
				MACs:         config.Server.MACs,
			},
		},
		catLimiter:  make(chan struct{}, config.Server.MaxConcurrentCats),
		tailLimiter: make(chan struct{}, config.Server.MaxConcurrentTails),
		sched:       newScheduler(),
		cont:        newContinuous(),
	}

	s.sshServerConfig.PasswordCallback = s.Callback
	s.sshServerConfig.PublicKeyCallback = server.PublicKeyCallback

	private, err := gossh.ParsePrivateKey(server.PrivateHostKey())
	if err != nil {
		dlog.Server.FatalPanic(err)
	}
	s.sshServerConfig.AddHostKey(private)

	return &s
}

// Start the server.
func (s *Server) Start(ctx context.Context) int {
	dlog.Server.Info("Starting server")
	bindAt := fmt.Sprintf("%s:%d", config.Server.SSHBindAddress, config.Common.SSHPort)
	dlog.Server.Info("Binding server", bindAt)

	listener, err := net.Listen("tcp", bindAt)
	if err != nil {
		dlog.Server.FatalPanic("Failed to open listening TCP socket", err)
	}

	go s.stats.start(ctx)
	go s.sched.start(ctx)
	go s.cont.start(ctx)
	go s.listenerLoop(ctx, listener)

	<-ctx.Done()
	// For future use.
	return 0
}

func (s *Server) listenerLoop(ctx context.Context, listener net.Listener) {
	dlog.Server.Debug("Starting listener loop")
	for {
		conn, err := listener.Accept() // Blocking
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			dlog.Server.Error("Failed to accept incoming connection", err)
			continue
		}

		if err := s.stats.serverLimitExceeded(); err != nil {
			dlog.Server.Error(err)
			conn.Close()
			continue
		}
		go s.handleConnection(ctx, conn)
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	dlog.Server.Info("Handling connection")

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.sshServerConfig)
	if err != nil {
		dlog.Server.Error("Something just happened", err)
		return
	}

	s.stats.incrementConnections()
	go gossh.DiscardRequests(reqs)
	for newChannel := range chans {
		go s.handleChannel(ctx, sshConn, newChannel)
	}
}

func (s *Server) handleChannel(ctx context.Context, sshConn gossh.Conn,
	newChannel gossh.NewChannel) {

	user, err := user.New(sshConn.User(), sshConn.RemoteAddr().String())
	if err != nil {
		dlog.Server.Error(user, err)
		if err := newChannel.Reject(gossh.Prohibited, err.Error()); err != nil {
			dlog.Server.Debug(err)
		}
		return
	}

	dlog.Server.Info(user, "Invoking channel handler")
	if newChannel.ChannelType() != "session" {
		err := errors.New("Don'w allow other channel types than session")
		dlog.Server.Error(user, err)
		if err := newChannel.Reject(gossh.Prohibited, err.Error()); err != nil {
			dlog.Server.Debug(err)
		}
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		dlog.Server.Error(user, "Could not accept channel", err)
		return
	}

	if err := s.handleRequests(ctx, sshConn, requests, channel, user); err != nil {
		dlog.Server.Error(user, err)
		sshConn.Close()
	}
}

func (s *Server) handleRequests(ctx context.Context, sshConn gossh.Conn,
	in <-chan *gossh.Request, channel gossh.Channel, user *user.User) error {

	dlog.Server.Info(user, "Invoking request handler")
	for req := range in {
		var payload = struct{ Value string }{}
		if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
			dlog.Server.Error(user, err)
		}

		switch req.Type {
		case "shell":
			var handler handlers.Handler
			switch user.Name {
			case config.HealthUser:
				handler = handlers.NewHealthHandler(user)
			default:
				handler = handlers.NewServerHandler(user, s.catLimiter, s.tailLimiter)
			}
			terminate := func() {
				handler.Shutdown()
				sshConn.Close()
			}

			go func() {
				defer terminate()
				// Broken pipe, cancel
				if _, err := io.Copy(channel, handler); err != nil {
					dlog.Server.Trace(user, fmt.Errorf("channel->handler: %w", err))
				}
			}()
			go func() {
				defer terminate()
				// Broken pipe, cancel
				if _, err := io.Copy(handler, channel); err != nil {
					dlog.Server.Trace(user, fmt.Errorf("handler->channel: %w", err))
				}
			}()
			go func() {
				select {
				case <-ctx.Done():
				case <-handler.Done():
				}
				terminate()
			}()
			go func() {
				if err := sshConn.Wait(); err != nil && err != io.EOF {
					dlog.Server.Error(user, err)
				}
				s.stats.decrementConnections()
				dlog.Server.Info(user, "Good bye Mister!")
				terminate()
			}()

			// Only serving shell type
			if err := req.Reply(true, nil); err != nil {
				dlog.Server.Trace(user, fmt.Errorf("reply(true): %w", err))
			}
		default:
			if err := req.Reply(false, nil); err != nil {
				dlog.Server.Trace(user, fmt.Errorf("reply(false): %w", err))
			}
			return fmt.Errorf("Closing SSH connection as unknown request received|%s|%v",
				req.Type, payload.Value)
		}
	}
	return nil
}

// Callback for SSH authentication.
func (s *Server) Callback(c gossh.ConnMetadata,
	authPayload []byte) (*gossh.Permissions, error) {

	user, err := user.New(c.User(), c.RemoteAddr().String())
	if err != nil {
		return nil, err
	}

	authInfo := string(authPayload)
	splitted := strings.Split(c.RemoteAddr().String(), ":")
	remoteIP := splitted[0]

	switch user.Name {
	case config.HealthUser:
		if authInfo == config.HealthUser {
			dlog.Server.Debug(user, "Granting permissions to health user")
			return nil, nil
		}
	case config.ScheduleUser:
		for _, job := range config.Server.Schedule {
			if s.backgroundCanSSH(user, authInfo, remoteIP, job.Name, job.AllowFrom) {
				dlog.Server.Debug(user, "Granting SSH connection")
				return nil, nil
			}
		}
	case config.ContinuousUser:
		for _, job := range config.Server.Continuous {
			if s.backgroundCanSSH(user, authInfo, remoteIP, job.Name, job.AllowFrom) {
				dlog.Server.Debug(user, "Granting SSH connection")
				return nil, nil
			}
		}
	default:
	}

	return nil, fmt.Errorf("user %s not authorized", user)
}

func (s *Server) backgroundCanSSH(user *user.User, jobName, remoteIP,
	allowedJobName string, allowFrom []string) bool {

	dlog.Server.Debug("backgroundCanSSH", user, jobName, remoteIP, allowedJobName, allowFrom)
	if jobName != allowedJobName {
		dlog.Server.Debug(user, jobName, "backgroundCanSSH",
			"Job name does not match, skipping to next one...", allowedJobName)
		return false
	}

	for _, myAddr := range allowFrom {
		ips, err := net.LookupIP(myAddr)
		if err != nil {
			dlog.Server.Debug(user, jobName, "backgroundCanSSH", "Unable to lookup IP "+
				"address for allowed hosts lookup, skipping to next one...", myAddr, err)
			continue
		}
		for _, ip := range ips {
			dlog.Server.Debug(user, jobName, "backgroundCanSSH", "Comparing IP addresses",
				remoteIP, ip.String())
			if remoteIP == ip.String() {
				return true
			}
		}
	}

	return false
}
