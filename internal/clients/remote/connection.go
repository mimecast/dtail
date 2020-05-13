package remote

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/ssh/client"

	"golang.org/x/crypto/ssh"
)

// Connection represents a client connection connection to a single server.
type Connection struct {
	// The remote server's hostname connected to.
	Server string
	// The remote server's port connected to.
	port int
	// The SSH client configuration used.
	config *ssh.ClientConfig
	// The SSH client handler to use.
	Handler handlers.Handler
	// DTail commands sent from client to server. When client loses
	// connection to the server it re-connects automatically and sends the
	// same commands again.
	Commands []string
	// Is it a persistent connection or a one-off?
	isOneOff bool
	// To deal with SSH server host keys
	hostKeyCallback client.HostKeyCallback
	// To determine if connection throttling has finished or not
	throttlingDone bool
}

// NewConnection returns a new connection.
func NewConnection(server string, userName string, authMethods []ssh.AuthMethod, hostKeyCallback client.HostKeyCallback) *Connection {
	logger.Debug(server, "Creating new connection")

	c := Connection{
		hostKeyCallback: hostKeyCallback,
		config: &ssh.ClientConfig{
			User:            userName,
			Auth:            authMethods,
			HostKeyCallback: hostKeyCallback.Wrap(),
			Timeout:         time.Second * 3,
		},
	}

	c.initServerPort(server)

	return &c
}

// NewOneOffConnection creates new one-off connection (only for sending a series of commands and then quit).
func NewOneOffConnection(server string, userName string, authMethods []ssh.AuthMethod) *Connection {
	c := Connection{
		config: &ssh.ClientConfig{
			User:            userName,
			Auth:            authMethods,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		},
		isOneOff: true,
	}

	c.initServerPort(server)

	return &c
}

// Attempt to parse the server port address from the provided server FQDN.
func (c *Connection) initServerPort(server string) {
	c.Server = server
	c.port = config.Common.SSHPort
	parts := strings.Split(server, ":")

	if len(parts) == 2 {
		logger.Debug("Parsing port from hostname", parts)
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			logger.FatalExit("Unable to parse client port", server, parts, err)
		}
		c.Server = parts[0]
		c.port = port
	}
}

// Start the server connection. Build up SSH session and send some DTail commands.
func (c *Connection) Start(ctx context.Context, cancel context.CancelFunc, throttleCh, statsCh chan struct{}) {
	// Throttle how many connections can be established concurrently (based on ch length)
	logger.Debug("Throttling connection", len(throttleCh), cap(throttleCh))

	select {
	case throttleCh <- struct{}{}:
	case <-ctx.Done():
		logger.Debug("Not establishing connection as context is done", len(throttleCh), cap(throttleCh), c.Server)
		return
	}

	logger.Debug("Throttling says that the connection can be established", len(throttleCh), cap(throttleCh), c.Server)

	go func() {
		defer func() {
			if !c.throttlingDone {
				logger.Debug("Unthrottling connection (1)", len(throttleCh), cap(throttleCh), c.Server)
				c.throttlingDone = true
				<-throttleCh
			}
			cancel()
		}()

		if err := c.dial(ctx, cancel, c.Server, c.port, throttleCh, statsCh); err != nil {
			logger.Warn(c.Server, c.port, err)
			if c.hostKeyCallback.Untrusted(fmt.Sprintf("%s:%d", c.Server, c.port)) {
				logger.Debug("Not trusting host", c.Server, c.port)
			}
		}
	}()

	<-ctx.Done()
}

// Dail into a new SSH connection. Close connection in case of an error.
func (c *Connection) dial(ctx context.Context, cancel context.CancelFunc, host string, port int, throttleCh, statsCh chan struct{}) error {
	logger.Debug("Incrementing connection stats", host)
	statsCh <- struct{}{}
	defer func() {
		logger.Debug("Decrementing connection stats", host)
		<-statsCh
	}()

	logger.Debug("Dialing into the connection", host)
	address := fmt.Sprintf("%s:%d", host, port)

	client, err := ssh.Dial("tcp", address, c.config)
	if err != nil {
		return err
	}
	defer client.Close()

	return c.session(ctx, cancel, client, throttleCh)
}

// Create the SSH session. Close the session in case of an error.
func (c *Connection) session(ctx context.Context, cancel context.CancelFunc, client *ssh.Client, throttleCh chan struct{}) error {
	logger.Debug(c.Server, "session")

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	return c.handle(ctx, cancel, session, throttleCh)
}

func (c *Connection) handle(ctx context.Context, cancel context.CancelFunc, session *ssh.Session, throttleCh chan struct{}) error {
	logger.Debug(c.Server, "handle")

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return err
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	if err := session.Shell(); err != nil {
		return err
	}

	go func() {
		defer cancel()
		io.Copy(stdinPipe, c.Handler)
	}()

	go func() {
		defer cancel()
		io.Copy(c.Handler, stdoutPipe)
	}()

	go func() {
		defer cancel()
		select {
		case <-c.Handler.Done():
		case <-ctx.Done():
		}
	}()

	// Send all commands to client.
	for _, command := range c.Commands {
		logger.Debug(command)
		c.Handler.SendMessage(command)
	}

	if !c.throttlingDone {
		logger.Debug("Unthrottling connection (2)", len(throttleCh), cap(throttleCh), c.Server)
		c.throttlingDone = true
		<-throttleCh
	}

	<-ctx.Done()
	return nil
}
