package connectors

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	sessionspec "github.com/mimecast/dtail/internal/session"
	"github.com/mimecast/dtail/internal/ssh/client"

	"golang.org/x/crypto/ssh"
)

// SSHSettings provides the connection settings needed by ServerConnection.
type SSHSettings interface {
	SSHPort() int
	// SSHConnectTimeout bounds the TCP connect.
	SSHConnectTimeout() time.Duration
	// SSHHandshakeTimeout bounds the SSH handshake after the TCP connect,
	// not counting the host key check (see handshakeDeadline).
	SSHHandshakeTimeout() time.Duration
}

type sshSession interface {
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.Reader, error)
	Shell() error
	Wait() error
	Close() error
}

type serverDialFunc func(context.Context, context.CancelFunc, chan struct{}, chan struct{}) error

type sshSessionIO struct {
	stdin      io.WriteCloser
	stdinDone  <-chan struct{}
	stdoutDone <-chan struct{}
	waitDone   <-chan struct{}
}

const (
	defaultSSHConnectTimeout    = 2 * time.Second
	defaultSSHHandshakeTimeout  = 30 * time.Second
	defaultSSHPort              = 2222
	defaultCapabilityWait       = 250 * time.Millisecond
	defaultSSHCloseDrainTimeout = 6 * time.Second
)

// ServerConnection represents a connection to a single remote dtail server via
// SSH protocol.
type ServerConnection struct {
	// The full server string as received from the server discovery (can be with port number)
	server string
	// Only the hostname or FQDN (without the port number)
	hostname string
	// Only the port number.
	port int
	// handshakeTimeout bounds the SSH handshake (see handshakeDeadline).
	handshakeTimeout time.Duration
	config           *ssh.ClientConfig
	handler          handlers.Handler
	commands         []string
	sessionSpec      sessionspec.Spec
	sessionState     committedSessionState
	interactive      bool
	authKeyPath      string
	authKeyDisabled  bool
	hostKeyCallback  client.HostKeyCallback
	logger           logging.Logger
	// dialFn is an optional seam for connection lifecycle tests.
	dialFn serverDialFunc
	// throttleReleased ensures the throttle slot is returned to throttleCh
	// exactly once, even if both the early-release path in handle() and the
	// deferred cleanup in Start() execute concurrently or the same goroutine
	// hits both paths.  sync.Once is safe for concurrent callers whereas the
	// previous bool guard was not synchronized.
	throttleReleased sync.Once
}

var _ Connector = (*ServerConnection)(nil)

// NewServerConnection returns a new DTail SSH server connection.
func NewServerConnection(server string, userName string,
	authMethods []ssh.AuthMethod, hostKeyCallback client.HostKeyCallback,
	handler handlers.Handler, commands []string, sessionSpec sessionspec.Spec,
	interactive bool, authKeyPath string, authKeyDisabled bool, settings SSHSettings,
	logger logging.Logger) (*ServerConnection, error) {

	logger = logging.OrNop(logger)
	logger.Debug(server, "Creating new connection", server, handler, config.RedactReadShares(commands))
	connectTimeout, handshakeTimeout, defaultPort := resolveSSHSettings(settings)

	c := ServerConnection{
		hostKeyCallback:  hostKeyCallback,
		server:           server,
		handler:          handler,
		commands:         commands,
		sessionSpec:      sessionSpec,
		interactive:      interactive,
		authKeyPath:      resolveAuthKeyPath(authKeyPath),
		authKeyDisabled:  authKeyDisabled,
		logger:           logger,
		handshakeTimeout: handshakeTimeout,
		config: &ssh.ClientConfig{
			User: userName,
			Auth: authMethods,
			// Timeout is the TCP connect timeout of dial(); the handshake
			// has its own (handshakeTimeout).
			Timeout: connectTimeout,
			// HostKeyCallback is assigned per-handshake in handshake() so
			// the callback can honour the handshake's context and pause
			// the handshake deadline (see handshake()).
		},
	}

	if err := c.initServerPort(defaultPort); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolveSSHSettings returns the connect and handshake timeouts and the
// default port of settings, with the defaults for nil settings and values
// that are not positive.
func resolveSSHSettings(settings SSHSettings) (time.Duration, time.Duration, int) {
	connectTimeout, handshakeTimeout, port := defaultSSHConnectTimeout, defaultSSHHandshakeTimeout, defaultSSHPort
	if settings == nil {
		return connectTimeout, handshakeTimeout, port
	}
	if timeout := settings.SSHConnectTimeout(); timeout > 0 {
		connectTimeout = timeout
	}
	if timeout := settings.SSHHandshakeTimeout(); timeout > 0 {
		handshakeTimeout = timeout
	}
	if settingsPort := settings.SSHPort(); settingsPort != 0 {
		port = settingsPort
	}
	return connectTimeout, handshakeTimeout, port
}

// Server returns the server hostname connected to.
func (c *ServerConnection) Server() string { return c.server }

func (c *ServerConnection) log() logging.Logger { return logging.OrNop(c.logger) }

// Handler returns the handler used for the connection.
func (c *ServerConnection) Handler() handlers.Handler { return c.handler }

// ApplySessionSpec starts or updates the interactive session state on the
// existing SSH connection.
func (c *ServerConnection) ApplySessionSpec(spec sessionspec.Spec, timeout time.Duration) error {
	return applySessionSpec(c.server, c.handler, &c.sessionState, spec, timeout, c.logger)
}

// ApplySessionSpecWithGeneration starts or updates the interactive session
// state using an explicit committed generation as the base for the update.
func (c *ServerConnection) ApplySessionSpecWithGeneration(spec sessionspec.Spec, generation uint64, timeout time.Duration) error {
	return applySessionSpecWithGeneration(c.server, c.handler, &c.sessionState, spec, generation, false, timeout, c.logger)
}

// CommittedSession returns the last server-acknowledged session state.
func (c *ServerConnection) CommittedSession() (sessionspec.Spec, uint64, bool) {
	return c.sessionState.snapshot()
}

// ReplaceStartSessionSpec makes spec the workload of the initial SESSION
// START if that has not begun yet (see Connector).
func (c *ServerConnection) ReplaceStartSessionSpec(spec sessionspec.Spec) (bool, error) {
	return c.sessionState.replaceStartSpec(spec)
}

// RestoreStartSessionSpec rolls the start spec back after a failed reload
// (see Connector).
func (c *ServerConnection) RestoreStartSessionSpec(spec sessionspec.Spec) (bool, error) {
	return c.sessionState.restoreStartSpec(spec, abandonStartWorkload(c.handler))
}

// Attempt to parse the server port address from the provided server FQDN.
func (c *ServerConnection) initServerPort(defaultPort int) error {
	hostname, port, err := ParseServerAddress(c.server, defaultPort)
	if err != nil {
		return err
	}
	c.hostname = hostname
	c.port = port
	return nil
}

// ParseServerAddress returns the host and the port of a server address as a
// connection dials them: host, host:port, an IPv6 address, or [ipv6]:port;
// an address without a port gets defaultPort.
func ParseServerAddress(address string, defaultPort int) (string, int, error) {
	if defaultPort < 1 || defaultPort > 65535 {
		return "", 0, fmt.Errorf("parse server address %q: default port must be between 1 and 65535", address)
	}
	if address == "" || address != strings.TrimSpace(address) {
		return "", 0, fmt.Errorf("parse server address %q: address is empty or contains surrounding whitespace", address)
	}

	switch {
	case strings.HasPrefix(address, "["):
		return parseBracketedAddress(address, defaultPort)
	case strings.Count(address, ":") > 1:
		// A bare IPv6 address: it can carry no port.
		if err := validateIPv6Host(address); err != nil {
			return "", 0, fmt.Errorf("parse server address %q: %w", address, err)
		}
		return address, defaultPort, nil
	case strings.Contains(address, ":"):
		return splitServerHostPort(address, validateServerHost)
	}
	if err := validateServerHost(address); err != nil {
		return "", 0, fmt.Errorf("parse server address %q: %w", address, err)
	}
	return address, defaultPort, nil
}

// parseBracketedAddress parses [ipv6], which gets defaultPort, and
// [ipv6]:port.
func parseBracketedAddress(address string, defaultPort int) (string, int, error) {
	if !strings.HasSuffix(address, "]") {
		return splitServerHostPort(address, validateIPv6Host)
	}
	hostname := address[1 : len(address)-1]
	if err := validateIPv6Host(hostname); err != nil {
		return "", 0, fmt.Errorf("parse server address %q: %w", address, err)
	}
	return hostname, defaultPort, nil
}

// splitServerHostPort splits address, a host:port or [ipv6]:port, and
// checks its host with validateHost and its port.
func splitServerHostPort(address string, validateHost func(string) error) (string, int, error) {
	hostname, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("parse server address %q: %w", address, err)
	}
	if err = validateHost(hostname); err != nil {
		return "", 0, fmt.Errorf("parse server address %q: %w", address, err)
	}
	port, err := parseServerPort(address, portText)
	if err != nil {
		return "", 0, err
	}
	return hostname, port, nil
}

func parseServerPort(address, portText string) (int, error) {
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("parse port in server address %q: %w", address, err)
	}
	if port == 0 {
		return 0, fmt.Errorf("parse port in server address %q: port must be between 1 and 65535", address)
	}
	return int(port), nil
}

func validateIPv6Host(hostname string) error {
	address, err := netip.ParseAddr(hostname)
	if err != nil || !address.Is6() {
		return fmt.Errorf("invalid IPv6 host %q", hostname)
	}
	return nil
}

func validateServerHost(hostname string) error {
	if hostname == "" {
		return fmt.Errorf("hostname is empty")
	}
	if strings.ContainsAny(hostname, "[]/\\") || strings.IndexFunc(hostname, func(r rune) bool {
		return r <= ' ' || r == 0x7f
	}) >= 0 {
		return fmt.Errorf("invalid hostname %q", hostname)
	}
	return nil
}

// Start the connection to the server. It returns once the connection
// ended; its handler is shut down by then, also when no session came up
// (see shutdownHandlerWithoutSession).
func (c *ServerConnection) Start(ctx context.Context, cancel context.CancelFunc,
	throttleCh, statsCh chan struct{}) {

	defer c.shutdownHandlerWithoutSession()

	// Throttle how many connections can be established concurrently (based on ch length)
	c.log().Debug(c.server, "Throttling connection", len(throttleCh), cap(throttleCh))

	select {
	case throttleCh <- struct{}{}:
	case <-ctx.Done():
		c.log().Debug(c.server, "Not establishing connection as context is done",
			len(throttleCh), cap(throttleCh))
		return
	}

	c.log().Debug(c.server, "Throttling says that the connection can be established",
		len(throttleCh), cap(throttleCh))

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			// Release the throttle slot on the way out regardless of which
			// code path already attempted it.  throttleReleased.Do guarantees
			// the drain happens exactly once even if handle() already released
			// the slot early (the fast path when the session is fully up).
			c.throttleReleased.Do(func() {
				c.log().Debug(c.server, "Unthrottling connection (cleanup)",
					len(throttleCh), cap(throttleCh))
				<-throttleCh
			})
			cancel()
		}()

		dial := c.dial
		if c.dialFn != nil {
			dial = c.dialFn
		}
		if err := dial(ctx, cancel, throttleCh, statsCh); err != nil {
			if shouldReportConnectionError(ctx, err) {
				c.handler.ReportServerError(err.Error())
			}
			if c.hostKeyCallback != nil && c.hostKeyCallback.Untrusted(c.server) {
				c.log().Debug(c.server, "Not trusting host")
			}
		}
	}()

	<-done
}

// shutdownHandlerWithoutSession shuts the handler down if Start ended
// without a session that did (finishSessionWork shuts down the handler of
// every session it ran): the context ended before a throttle slot was
// free, or the dial, the SSH handshake, the session or its shell failed.
// Otherwise whoever waits on the handler would wait for a session that
// never comes: a probe waiting for the capabilities frame held its probe
// round (and so :probe) and every path listing for the whole round timeout
// when the first server in --servers was down, and a :reload waited until
// sending its SESSION UPDATE timed out. A retrying client (dtail follow
// mode) replaces the connection, and with it the handler, before the next
// attempt, so the shutdown does not affect its reconnects. The handler's
// status keeps the connection error reported by Start.
func (c *ServerConnection) shutdownHandlerWithoutSession() {
	select {
	case <-c.handler.Done():
	default:
		c.handler.Shutdown()
	}
}

// Dail into a new SSH connection. Close connection in case of an error.
func (c *ServerConnection) dial(ctx context.Context, cancel context.CancelFunc,
	throttleCh, statsCh chan struct{}) error {

	c.log().Debug(c.server, "Incrementing connection stats")
	statsCh <- struct{}{}
	defer func() {
		c.log().Debug(c.server, "Decrementing connection stats")
		<-statsCh
	}()

	address := net.JoinHostPort(c.hostname, strconv.Itoa(c.port))
	c.log().Debug(c.server, "Dialing into the connection", address)

	// Use context-aware dialing to enable proper cancellation during connection establishment.
	// TCP KeepAlive (30s) prevents silent connection failures on long-lived connections.
	dialer := &net.Dialer{
		Timeout:   c.config.Timeout, // The connect timeout (Common.SSHConnectTimeoutMs)
		KeepAlive: 30 * time.Second, // Standard Go default for connection health monitoring
	}

	// Establish TCP connection with context support for cancellation
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return preferContextError(ctx, fmt.Errorf("failed to dial TCP connection to %s: %w", address, err))
	}
	stopContextClose := context.AfterFunc(ctx, func() {
		if closeErr := conn.Close(); closeErr != nil {
			c.log().Trace(closeErr)
		}
	})
	defer stopContextClose()

	client, err := c.handshake(ctx, conn, address)
	if err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			c.log().Trace(closeErr)
		}
		return err
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			c.log().Trace(closeErr)
		}
	}()

	return c.session(ctx, cancel, client, throttleCh)
}

// handshake performs the SSH handshake over the established TCP connection
// within c.handshakeTimeout, not counting the host key check, which may ask
// the user (see handshakeDeadline). It builds a per-handshake
// ssh.ClientConfig so the host key callback is bound to ctx and unblocks
// cleanly if the handshake is cancelled (e.g. when the user aborts before
// answering the unknown host question). The caller closes conn on an error.
func (c *ServerConnection) handshake(ctx context.Context, conn net.Conn, address string) (*ssh.Client, error) {
	deadline, err := startHandshakeDeadline(conn, c.handshakeTimeout)
	if err != nil {
		return nil, preferContextError(ctx, fmt.Errorf("SSH handshake failed for %s: %w", address, err))
	}
	handshakeConfig := *c.config
	handshakeConfig.HostKeyCallback = deadline.wrapHostKeyCallback(c.hostKeyCallback.Wrap(ctx))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, &handshakeConfig)
	expired, clearErr := deadline.finish()
	if err != nil {
		if expired {
			err = fmt.Errorf("SSH handshake failed for %s: no handshake within %v: %w",
				address, c.handshakeTimeout, err)
		} else {
			err = fmt.Errorf("SSH handshake failed for %s: %w", address, err)
		}
		return nil, preferContextError(ctx, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	if clearErr != nil {
		if closeErr := client.Close(); closeErr != nil {
			c.log().Trace(closeErr)
		}
		return nil, preferContextError(ctx,
			fmt.Errorf("clear SSH handshake deadline for %s: %w", address, clearErr))
	}
	return client, nil
}

// Create the SSH session. Close the session in case of an error.
func (c *ServerConnection) session(ctx context.Context, cancel context.CancelFunc,
	client *ssh.Client, throttleCh chan struct{}) error {

	c.log().Debug(c.server, "Creating SSH session")
	session, err := client.NewSession()
	if err != nil {
		return preferContextError(ctx, fmt.Errorf("failed to create SSH session for %s: %w", c.server, err))
	}
	return c.handle(ctx, cancel, session, throttleCh)
}

func (c *ServerConnection) handle(ctx context.Context, cancel context.CancelFunc,
	session sshSession, throttleCh chan struct{}) error {
	closeSession := c.closeSessionOnce(session)
	defer closeSession()

	c.log().Debug(c.server, "Creating handler for SSH session")
	sessionIO, err := c.startSessionIO(ctx, session)
	if err != nil {
		return err
	}
	c.registerAuthKey()

	dispatchErr := dispatchInitialCommands(ctx, c.server, c.handler, c.commands, c.interactive,
		c.sessionSpec, &c.sessionState, c.log())
	if dispatchErr != nil {
		dispatchErr = preferContextError(ctx, dispatchErr)
	}
	c.finishSessionWork(ctx, cancel, closeSession, sessionIO, throttleCh, dispatchErr)
	return dispatchErr
}

func (c *ServerConnection) closeSessionOnce(session sshSession) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := session.Close(); err != nil {
				c.log().Trace(err)
			}
		})
	}
}

func (c *ServerConnection) startSessionIO(ctx context.Context, session sshSession) (*sshSessionIO, error) {
	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return nil, preferContextError(ctx,
			fmt.Errorf("failed to get SSH session stdin pipe for %s: %w", c.server, err))
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return nil, preferContextError(ctx,
			fmt.Errorf("failed to get SSH session stdout pipe for %s: %w", c.server, err))
	}
	if shellErr := session.Shell(); shellErr != nil {
		return nil, preferContextError(ctx,
			fmt.Errorf("failed to start SSH shell for %s: %w", c.server, shellErr))
	}

	return &sshSessionIO{
		stdin:      stdinPipe,
		stdinDone:  copyAsync(stdinPipe, c.handler, c.log()),
		stdoutDone: copyAsync(c.handler, stdoutPipe, c.log()),
		waitDone:   waitSessionAsync(session, c.log()),
	}, nil
}

func (c *ServerConnection) registerAuthKey() {
	if c.authKeyDisabled {
		c.log().Debug(c.server, "Skipping AUTHKEY registration because auth-key is disabled")
		return
	}
	c.sendAuthKeyRegistrationCommand()
}

func (c *ServerConnection) finishSessionWork(ctx context.Context, cancel context.CancelFunc,
	closeSession func(), sessionIO *sshSessionIO, throttleCh chan struct{}, dispatchErr error) {
	if dispatchErr != nil {
		cancel()
		closeSession()
	} else {
		c.releaseSessionThrottle(throttleCh)
		c.waitForSessionEnd(ctx, cancel, closeSession, sessionIO)
	}

	// Closing the transport above interrupts a blocked stdout read. Joining the
	// copy before Shutdown makes the MapReduce flush final: no subsequent Write
	// can leave local aggregate state behind after it has been flushed.
	<-sessionIO.stdoutDone
	c.handler.Shutdown()

	// Shutdown releases a handler.Read blocked waiting for another command.
	// Closing the SSH stdin pipe also interrupts a write if the peer has stopped
	// reading. Join both remaining session goroutines before returning so callers
	// can safely render final results.
	if closeErr := sessionIO.stdin.Close(); closeErr != nil {
		c.log().Trace(closeErr)
	}
	<-sessionIO.stdinDone
	closeSession()
	<-sessionIO.waitDone
	cancel()
}

func (c *ServerConnection) releaseSessionThrottle(throttleCh chan struct{}) {
	// Release the throttle slot as soon as the session is fully established so
	// the next pending connection can proceed without waiting for this session
	// to finish. throttleReleased.Do remains idempotent with Start's cleanup.
	c.throttleReleased.Do(func() {
		c.log().Debug(c.server, "Unthrottling connection (session up)",
			len(throttleCh), cap(throttleCh))
		<-throttleCh
	})
}

func (c *ServerConnection) waitForSessionEnd(ctx context.Context, cancel context.CancelFunc,
	closeSession func(), sessionIO *sshSessionIO) {
	select {
	case <-ctx.Done():
		closeSession()
	case <-c.handler.Done():
		c.waitForCloseAcknowledgement(ctx, sessionIO)
		cancel()
		closeSession()
	case <-sessionIO.stdinDone:
		cancel()
		closeSession()
	case <-sessionIO.stdoutDone:
		// Stdout EOF does not guarantee that the peer also closed the SSH
		// channel or request stream. Cancel the connection context so dial's
		// context hook closes the transport and unblocks Session.Wait.
		cancel()
		closeSession()
	case <-sessionIO.waitDone:
	}
}

func (c *ServerConnection) waitForCloseAcknowledgement(ctx context.Context, sessionIO *sshSessionIO) {
	// A hidden close request marks the handler done after enqueueing its
	// acknowledgement. Give the stdin copy a bounded opportunity to send that
	// acknowledgement before closing the transport.
	timer := time.NewTimer(defaultSSHCloseDrainTimeout)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-sessionIO.stdinDone:
	case <-sessionIO.stdoutDone:
	case <-ctx.Done():
	case <-timer.C:
	}
}

func preferContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// shouldReportConnectionError reports whether err, which ended a connection,
// still has to be reported to the user. Missing journal support was already
// reported by requireJournalCapability, and a canceled context is no error of
// the connection.
func shouldReportConnectionError(ctx context.Context, err error) bool {
	if errors.Is(err, ErrJournalUnsupported) {
		return false
	}
	ctxErr := ctx.Err()
	return ctxErr == nil || !errors.Is(err, ctxErr)
}

func copyAsync(dst io.Writer, src io.Reader, logger logging.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(dst, src); err != nil {
			logger.Trace(err)
		}
	}()
	return done
}

func waitSessionAsync(session sshSession, logger logging.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := session.Wait(); err != nil {
			logger.Trace(err)
		}
	}()
	return done
}

// resolveAuthKeyPath returns the effective auth-key path. When the provided
// path is non-empty it is used as-is. Otherwise the function falls back to
// $HOME/.ssh/id_rsa. If HOME is also empty it returns "" so that the AUTHKEY
// registration step (sendAuthKeyRegistrationCommand) will skip gracefully
// instead of trying to open a path that the SSH library cannot expand.
func resolveAuthKeyPath(authKeyPath string) string {
	if strings.TrimSpace(authKeyPath) != "" {
		return authKeyPath
	}
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		return ""
	}
	return filepath.Join(homeDir, ".ssh", "id_rsa")
}

func (c *ServerConnection) sendAuthKeyRegistrationCommand() {
	authKeyPubPath := c.authKeyPath + ".pub"
	authKeyPubBytes, err := os.ReadFile(authKeyPubPath)
	if err != nil {
		c.log().Debug(c.server, "Skipping AUTHKEY registration, unable to read public key", authKeyPubPath, err)
		return
	}

	authKeyBase64, err := extractAuthKeyBase64(authKeyPubBytes)
	if err != nil {
		c.log().Debug(c.server, "Skipping AUTHKEY registration, invalid public key file", authKeyPubPath, err)
		return
	}

	if sendErr := c.handler.SendMessage("AUTHKEY " + authKeyBase64); sendErr != nil {
		c.log().Debug(c.server, "Unable to send AUTHKEY registration command", sendErr)
		return
	}
	c.log().Debug(c.server, "Sent AUTHKEY registration command", authKeyPubPath)
}

func extractAuthKeyBase64(authKeyPubBytes []byte) (string, error) {
	authKeyPubContent := string(authKeyPubBytes)
	for _, line := range strings.Split(authKeyPubContent, "\n") {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" || strings.HasPrefix(trimmedLine, "#") {
			continue
		}

		fields := strings.Fields(trimmedLine)
		if len(fields) < 2 {
			return "", fmt.Errorf("expected authorized key format '<type> <base64-key> [comment]'")
		}

		authKeyBase64 := strings.TrimSpace(fields[1])
		if _, err := base64.StdEncoding.DecodeString(authKeyBase64); err != nil {
			return "", fmt.Errorf("invalid base64 public key: %w", err)
		}

		return authKeyBase64, nil
	}

	return "", fmt.Errorf("no public key found")
}
