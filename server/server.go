package server

import (
	"dtail/config"
	"dtail/logger"
	"dtail/server/handlers"
	"dtail/server/user"
	"dtail/ssh/server"
	"dtail/version"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// Server is the main server data structure.
type Server struct {
	// Various server statistics counters.
	stats stats
	// SSH server configuration.
	sshServerConfig *gossh.ServerConfig
	// To control the max amount of concurrent cats (which can cause a lot of I/O on the server)
	catLimiterCh chan struct{}
	// To control the max amount of concurrent tails
	tailLimiterCh chan struct{}
	// Ask to shutdown the server
	stop chan struct{}
}

// New returns a new server.
func New() *Server {
	logger.Info("Creating server", version.String())

	s := Server{
		sshServerConfig: &gossh.ServerConfig{},
		catLimiterCh:    make(chan struct{}, config.Server.MaxConcurrentCats),
		tailLimiterCh:   make(chan struct{}, config.Server.MaxConcurrentTails),
		stop:            make(chan struct{}),
	}

	s.sshServerConfig.PasswordCallback = s.controlUserCallback
	s.sshServerConfig.PublicKeyCallback = server.PublicKeyCallback

	private, err := gossh.ParsePrivateKey(server.PrivateHostKey())
	if err != nil {
		logger.FatalExit(err)
	}
	s.sshServerConfig.AddHostKey(private)

	return &s
}

// Start the server.
func (s *Server) Start(wg *sync.WaitGroup) int {
	defer wg.Done()
	logger.Info("Starting server")

	bindAt := fmt.Sprintf("%s:%d", config.Server.SSHBindAddress, config.Common.SSHPort)
	logger.Info("Binding server", bindAt)
	listener, err := net.Listen("tcp", bindAt)
	if err != nil {
		logger.FatalExit("Failed to open listening TCP socket", err)
	}

	go s.stats.periodicLogServerStats(s.stop)

	for {
		conn, err := listener.Accept() // Blocking
		if err != nil {
			logger.Error("Failed to accept incoming connection", err)
			continue
		}

		if err := s.stats.serverLimitExceeded(); err != nil {
			logger.Error(err)
			conn.Close()
			continue
		}

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	logger.Info("Handling connection")

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.sshServerConfig)
	if err != nil {
		logger.Error("Something just happened", err)
		return
	}

	s.stats.incrementConnections()

	go gossh.DiscardRequests(reqs)
	for newChannel := range chans {
		go s.handleChannel(sshConn, newChannel)
	}
}

func (s *Server) handleChannel(sshConn gossh.Conn, newChannel gossh.NewChannel) {
	user := user.New(sshConn.User(), sshConn.RemoteAddr().String())
	logger.Info(user, "Invoking channel handler")

	if newChannel.ChannelType() != "session" {
		err := errors.New("Don'w allow other channel types than session")
		logger.Error(user, err)
		newChannel.Reject(gossh.Prohibited, err.Error())
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		logger.Error(user, "Could not accept channel", err)
		return
	}

	if err := s.handleRequests(sshConn, requests, channel, user); err != nil {
		logger.Error(user, err)
		sshConn.Close()
	}
}

func (s *Server) handleRequests(sshConn gossh.Conn, in <-chan *gossh.Request, channel gossh.Channel, user *user.User) error {
	logger.Info(user, "Invoking request handler")

	for req := range in {
		var payload = struct{ Value string }{}
		gossh.Unmarshal(req.Payload, &payload)

		switch req.Type {
		case "shell":
			var handler handlers.Handler
			switch user.Name {
			case config.ControlUser:
				handler = handlers.NewControlHandler(user)
			default:
				handler = handlers.NewServerHandler(user, s.catLimiterCh, s.tailLimiterCh)
			}

			// Bi-directionally connect SSH stream to SSH handler
			brokenPipe1 := make(chan struct{})
			go func() {
				defer close(brokenPipe1)
				io.Copy(channel, handler)
			}()

			brokenPipe2 := make(chan struct{})
			go func() {
				defer close(brokenPipe2)
				io.Copy(handler, channel)
			}()

			// Ensure to close all fd's and stop all goroutines once ssh connection terminated
			go func() {
				defer s.stats.decrementConnections()
				defer handler.Close()

				if err := sshConn.Wait(); err != nil && err != io.EOF {
					logger.Error(user, err)
				}
				logger.Info(user, "Good bye Mister!")
			}()

			// Close the underlying ssh socket when server shuts down
			go func() {
				select {
				case <-s.stop:
					logger.Debug(user, "Server initiating shutdown on handler")
				case <-handler.Wait():
					logger.Debug(user, "Handler initiating shutdown by its own")
				case <-brokenPipe1:
					logger.Debug(user, "Broken pipe1")
				case <-brokenPipe2:
					logger.Debug(user, "Broken pipe2")
				}
				sshConn.Close()
				logger.Info(user, "Closed SSH connection")
			}()

			// Only serving shell type
			req.Reply(true, nil)

		default:
			req.Reply(false, nil)

			return fmt.Errorf("Closing SSH connection as unknown request recieved|%s|%v",
				req.Type, payload.Value)
		}
	}

	return nil
}

func (*Server) controlUserCallback(c gossh.ConnMetadata, authPayload []byte) (*gossh.Permissions, error) {
	user := user.New(c.User(), c.RemoteAddr().String())

	if user.Name == config.ControlUser && string(authPayload) == config.ControlUser {
		logger.Debug(user, "Initiating master control program")
		return nil, nil
	}

	return nil, fmt.Errorf("Not authorized")
}

// Stop the server.
func (s *Server) Stop() {
	close(s.stop)
	s.stats.waitForConnections()
}
