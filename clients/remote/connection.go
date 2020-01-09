package remote

import (
	"dtail/clients/handlers"
	"dtail/config"
	"dtail/logger"
	"dtail/ssh/client"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

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
	// Used to stop the connection
	stop chan struct{}
	// To deal with SSH server host keys
	hostKeyCallback *client.HostKeyCallback
}

// NewConnection returns a new connection.
func NewConnection(server string, userName string, authMethods []ssh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *Connection {
	logger.Debug(server, "Creating new connection")

	c := Connection{
		hostKeyCallback: hostKeyCallback,
		config: &ssh.ClientConfig{
			User:            userName,
			Auth:            authMethods,
			HostKeyCallback: hostKeyCallback.Wrap(),
			Timeout:         time.Second * 3,
		},
		stop: make(chan struct{}),
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
		stop:     make(chan struct{}),
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

// Start the server connection. Build up SSH session and send some DTail commandc.
func (c *Connection) Start(throttleCh, statsCh chan struct{}) {
	select {
	case <-c.stop:
		logger.Info(c.Server, c.port, "Disconnecting client")
		return
	default:
	}

	// Wait for SSH connection throttler
	throttleCh <- struct{}{}

	// Wait until connection has been initiated or an error occured
	// during initialization.
	throttleStopCh := make(chan struct{}, 2)
	go func() {
		<-throttleStopCh
		<-throttleCh
	}()

	if err := c.dial(c.Server, c.port, throttleStopCh, statsCh); err != nil {
		logger.Warn(c.Server, c.port, err)
		throttleStopCh <- struct{}{}

		if c.hostKeyCallback.Untrusted(fmt.Sprintf("%s:%d", c.Server, c.port)) {
			logger.Debug("Not trusting host, not trying to re-connect", c.Server, c.port)
			return
		}
	}
}

// Dail into a new SSH connection. Close connection in case of an error.
func (c *Connection) dial(host string, port int, throttleStopCh, statsCh chan struct{}) error {
	statsCh <- struct{}{}
	defer func() { <-statsCh }()

	logger.Debug(host, "dial")
	address := fmt.Sprintf("%s:%d", host, port)

	client, err := ssh.Dial("tcp", address, c.config)
	if err != nil {
		return err
	}
	defer client.Close()

	return c.session(client, throttleStopCh)
}

// Create the SSH session. Close the session in case of an error.
func (c *Connection) session(client *ssh.Client, throttleStopCh chan<- struct{}) error {
	logger.Debug(c.Server, "session")

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	return c.handle(session, throttleStopCh)
}

// Handle the SSH session. Also send periodic pings to the server in order
// to determine that session is still intact.
func (c *Connection) handle(session *ssh.Session, throttleStopCh chan<- struct{}) error {
	defer c.Handler.Stop()

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

	// Establish Bi-directional pipe between SSH session and client handler.
	brokenStdinPipe := make(chan struct{})
	go func() {
		defer close(brokenStdinPipe)
		io.Copy(stdinPipe, c.Handler)
	}()

	brokenStdoutPipe := make(chan struct{})
	go func() {
		defer close(brokenStdoutPipe)
		io.Copy(c.Handler, stdoutPipe)
	}()

	// SSH session established, other goroutine can initiate session now.
	throttleStopCh <- struct{}{}

	// Send all commands to client.
	for _, command := range c.Commands {
		logger.Debug(command)
		c.Handler.SendCommand(command)
	}

	if !c.isOneOff {
		return c.periodicAliveCheck(brokenStdinPipe, brokenStdoutPipe)
	}

	<-c.stop

	// Normal shutdown, all fine
	return nil
}

// Periodically check whether connection is still alive or not.
func (c *Connection) periodicAliveCheck(brokenStdinPipe, brokenStdoutPipe <-chan struct{}) error {
	for {
		select {
		case <-time.After(time.Second * 3):
			if err := c.Handler.Ping(); err != nil {
				return err
			}
		case <-brokenStdinPipe:
			logger.Debug("Broken stdin pipe", c.Server, c.port)
			return nil
		case <-brokenStdoutPipe:
			logger.Debug("Broken stdout pipe", c.Server, c.port)
			return nil
		case <-c.stop:
			return nil
		}
	}
}

// Stop the connection.
func (c *Connection) Stop() {
	close(c.stop)
}
