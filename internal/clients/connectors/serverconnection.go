package connectors

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/ssh/client"

	"golang.org/x/crypto/ssh"
)

// ServerConnection represents a connection to a single remote dtail server via SSH protocol.
type ServerConnection struct {
	server          string
	port            int
	config          *ssh.ClientConfig
	handler         handlers.Handler
	commands        []string
	isOneOff        bool
	hostKeyCallback client.HostKeyCallback
	throttlingDone  bool
}

// NewServerConnection returns a new DTail SSH server connection.
func NewServerConnection(server string, userName string, authMethods []ssh.AuthMethod, hostKeyCallback client.HostKeyCallback, handler handlers.Handler, commands []string) *ServerConnection {
	dlog.Client.Debug(server, "Creating new connection", server, handler, commands)

	c := ServerConnection{
		hostKeyCallback: hostKeyCallback,
		server:          server,
		handler:         handler,
		commands:        commands,
		config: &ssh.ClientConfig{
			User:            userName,
			Auth:            authMethods,
			HostKeyCallback: hostKeyCallback.Wrap(),
			Timeout:         time.Second * 2,
		},
	}

	c.initServerPort()
	return &c
}

// NewOneOffServerConnection creates new one-off connection (only for sending a series of commands and then quit).
func NewOneOffServerConnection(server string, userName string, authMethods []ssh.AuthMethod, handler handlers.Handler, commands []string) *ServerConnection {
	c := ServerConnection{
		server:   server,
		handler:  handler,
		commands: commands,
		config: &ssh.ClientConfig{
			User:            userName,
			Auth:            authMethods,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		},
		isOneOff: true,
	}

	c.initServerPort()
	return &c
}

func (c *ServerConnection) Server() string {
	return c.server
}

func (c *ServerConnection) Handler() handlers.Handler {
	return c.handler
}

// Attempt to parse the server port address from the provided server FQDN.
func (c *ServerConnection) initServerPort() {
	c.port = config.Common.SSHPort
	parts := strings.Split(c.server, ":")

	if len(parts) == 2 {
		dlog.Client.Debug("Parsing port from hostname", parts)
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			dlog.Client.FatalPanic("Unable to parse client port", c.server, parts, err)
		}
		c.server = parts[0]
		c.port = port
	}
}

func (c *ServerConnection) Start(ctx context.Context, cancel context.CancelFunc, throttleCh, statsCh chan struct{}) {
	// Throttle how many connections can be established concurrently (based on ch length)
	dlog.Client.Debug(c.server, "Throttling connection", len(throttleCh), cap(throttleCh))

	select {
	case throttleCh <- struct{}{}:
	case <-ctx.Done():
		dlog.Client.Debug(c.server, "Not establishing connection as context is done", len(throttleCh), cap(throttleCh))
		return
	}

	dlog.Client.Debug(c.server, "Throttling says that the connection can be established", len(throttleCh), cap(throttleCh))

	go func() {
		defer func() {
			if !c.throttlingDone {
				dlog.Client.Debug(c.server, "Unthrottling connection (1)", len(throttleCh), cap(throttleCh))
				c.throttlingDone = true
				<-throttleCh
			}
			cancel()
		}()

		if err := c.dial(ctx, cancel, throttleCh, statsCh); err != nil {
			dlog.Client.Warn(c.server, c.port, err)
			if c.hostKeyCallback.Untrusted(fmt.Sprintf("%s:%d", c.server, c.port)) {
				dlog.Client.Debug(c.server, "Not trusting host")
			}
		}
	}()

	<-ctx.Done()
}

// Dail into a new SSH connection. Close connection in case of an error.
func (c *ServerConnection) dial(ctx context.Context, cancel context.CancelFunc, throttleCh, statsCh chan struct{}) error {
	dlog.Client.Debug(c.server, "Incrementing connection stats")
	statsCh <- struct{}{}
	defer func() {
		dlog.Client.Debug(c.server, "Decrementing connection stats")
		<-statsCh
	}()

	dlog.Client.Debug(c.server, "Dialing into the connection")
	address := fmt.Sprintf("%s:%d", c.server, c.port)

	client, err := ssh.Dial("tcp", address, c.config)
	if err != nil {
		return err
	}
	defer client.Close()

	return c.session(ctx, cancel, client, throttleCh)
}

// Create the SSH session. Close the session in case of an error.
func (c *ServerConnection) session(ctx context.Context, cancel context.CancelFunc, client *ssh.Client, throttleCh chan struct{}) error {
	dlog.Client.Debug(c.server, "Creating SSH session")

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	return c.handle(ctx, cancel, session, throttleCh)
}

func (c *ServerConnection) handle(ctx context.Context, cancel context.CancelFunc, session *ssh.Session, throttleCh chan struct{}) error {
	dlog.Client.Debug(c.server, "Creating handler for SSH session")

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
		io.Copy(stdinPipe, c.handler)
		cancel()
	}()

	go func() {
		io.Copy(c.handler, stdoutPipe)
		cancel()
	}()

	go func() {
		select {
		case <-c.handler.Done():
		case <-ctx.Done():
		}
		cancel()
	}()

	// Send all commands to client.
	for _, command := range c.commands {
		dlog.Client.Debug(command)
		c.handler.SendMessage(command)
	}

	if !c.throttlingDone {
		dlog.Client.Debug(c.server, "Unthrottling connection (2)", len(throttleCh), cap(throttleCh))
		c.throttlingDone = true
		<-throttleCh
	}

	<-ctx.Done()
	c.handler.Shutdown()

	return nil
}
