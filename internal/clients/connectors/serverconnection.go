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
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"
	"github.com/mimecast/dtail/internal/ssh/client"

	"golang.org/x/crypto/ssh"
)

// SSHSettings provides the connection settings needed by ServerConnection.
type SSHSettings interface {
	SSHPort() int
	SSHConnectTimeout() time.Duration
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
	port            int
	config          *ssh.ClientConfig
	handler         handlers.Handler
	commands        []string
	sessionSpec     sessionspec.Spec
	sessionState    committedSessionState
	interactive     bool
	authKeyPath     string
	authKeyDisabled bool
	hostKeyCallback client.HostKeyCallback
	logger          logging.Logger
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
	logger.Debug(server, "Creating new connection", server, handler, commands)
	sshConnectTimeout := defaultSSHConnectTimeout
	defaultPort := defaultSSHPort
	if settings != nil {
		sshConnectTimeout = settings.SSHConnectTimeout()
		defaultPort = settings.SSHPort()
	}
	if sshConnectTimeout <= 0 {
		sshConnectTimeout = defaultSSHConnectTimeout
	}
	if defaultPort == 0 {
		defaultPort = defaultSSHPort
	}

	c := ServerConnection{
		hostKeyCallback: hostKeyCallback,
		server:          server,
		handler:         handler,
		commands:        commands,
		sessionSpec:     sessionSpec,
		interactive:     interactive,
		authKeyPath:     resolveAuthKeyPath(authKeyPath),
		authKeyDisabled: authKeyDisabled,
		logger:          logger,
		config: &ssh.ClientConfig{
			User:    userName,
			Auth:    authMethods,
			Timeout: sshConnectTimeout,
			// HostKeyCallback is assigned per-handshake in dial() so the
			// callback can honour the handshake's context (see dial()).
		},
	}

	if err := c.initServerPort(defaultPort); err != nil {
		return nil, err
	}
	return &c, nil
}

// Server returns the server hostname connected to.
func (c *ServerConnection) Server() string { return c.server }

func (c *ServerConnection) log() logging.Logger { return logging.OrNop(c.logger) }

// Handler returns the handler used for the connection.
func (c *ServerConnection) Handler() handlers.Handler { return c.handler }

// SupportsQueryUpdates reports whether the remote server advertised the
// runtime query replacement capability. Older servers simply time out and
// return false here without affecting the legacy command path.
func (c *ServerConnection) SupportsQueryUpdates(timeout time.Duration) bool {
	return supportsQueryUpdates(c.handler, timeout)
}

// ApplySessionSpec starts or updates the interactive session state on the
// existing SSH connection when runtime query updates are supported.
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

// RestoreCommittedSession resets the local session snapshot without advancing
// the generation.
func (c *ServerConnection) RestoreCommittedSession(spec sessionspec.Spec, generation uint64, committed bool) {
	c.sessionState.restore(spec, generation, committed)
}

// Attempt to parse the server port address from the provided server FQDN.
func (c *ServerConnection) initServerPort(defaultPort int) error {
	hostname, port, err := parseServerAddress(c.server, defaultPort)
	if err != nil {
		return err
	}
	c.hostname = hostname
	c.port = port
	return nil
}

func parseServerAddress(address string, defaultPort int) (string, int, error) {
	if defaultPort < 1 || defaultPort > 65535 {
		return "", 0, fmt.Errorf("parse server address %q: default port must be between 1 and 65535", address)
	}
	if address == "" || address != strings.TrimSpace(address) {
		return "", 0, fmt.Errorf("parse server address %q: address is empty or contains surrounding whitespace", address)
	}

	if strings.HasPrefix(address, "[") {
		if strings.HasSuffix(address, "]") {
			hostname := address[1 : len(address)-1]
			if err := validateIPv6Host(hostname); err != nil {
				return "", 0, fmt.Errorf("parse server address %q: %w", address, err)
			}
			return hostname, defaultPort, nil
		}

		hostname, portText, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return "", 0, fmt.Errorf("parse server address %q: %w", address, splitErr)
		}
		if validateErr := validateIPv6Host(hostname); validateErr != nil {
			return "", 0, fmt.Errorf("parse server address %q: %w", address, validateErr)
		}
		port, portErr := parseServerPort(address, portText)
		if portErr != nil {
			return "", 0, portErr
		}
		return hostname, port, nil
	}

	if strings.Count(address, ":") > 1 {
		if validateErr := validateIPv6Host(address); validateErr != nil {
			return "", 0, fmt.Errorf("parse server address %q: %w", address, validateErr)
		}
		return address, defaultPort, nil
	}

	if strings.Contains(address, ":") {
		hostname, portText, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return "", 0, fmt.Errorf("parse server address %q: %w", address, splitErr)
		}
		if validateErr := validateServerHost(hostname); validateErr != nil {
			return "", 0, fmt.Errorf("parse server address %q: %w", address, validateErr)
		}
		port, portErr := parseServerPort(address, portText)
		if portErr != nil {
			return "", 0, portErr
		}
		return hostname, port, nil
	}

	if validateErr := validateServerHost(address); validateErr != nil {
		return "", 0, fmt.Errorf("parse server address %q: %w", address, validateErr)
	}
	return address, defaultPort, nil
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

// Start the connection to the server.
func (c *ServerConnection) Start(ctx context.Context, cancel context.CancelFunc,
	throttleCh, statsCh chan struct{}) {

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
		Timeout:   c.config.Timeout, // Use the SSH config timeout (2 seconds)
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

	// Perform SSH handshake over the established TCP connection. Build a
	// per-handshake ssh.ClientConfig so the host-key callback is bound to
	// ctx and unblocks cleanly if the handshake is cancelled (e.g. when the
	// user aborts before responding to the unknown-host prompt).
	handshakeConfig := *c.config
	handshakeConfig.HostKeyCallback = c.hostKeyCallback.Wrap(ctx)
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, &handshakeConfig)
	if err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			c.log().Trace(closeErr)
		}
		return preferContextError(ctx, fmt.Errorf("SSH handshake failed for %s: %w", address, err))
	}

	// Create SSH client from the connection components
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			c.log().Trace(closeErr)
		}
	}()

	return c.session(ctx, cancel, client, throttleCh)
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

	dispatchErr := dispatchInitialCommands(c.server, c.handler, c.commands, c.interactive,
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

func supportsQueryUpdates(handler handlers.Handler, timeout time.Duration) bool {
	if handler == nil {
		return false
	}

	if timeout <= 0 {
		timeout = defaultCapabilityWait
	}
	if !handler.WaitForCapabilities(timeout) {
		return false
	}

	return handler.HasCapability(protocol.CapabilityQueryUpdateV1)
}
