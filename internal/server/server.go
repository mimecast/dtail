package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
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
	// To control the max amount of concurrent cats (which can cause a lot of I/O on the server)
	catLimiterCh chan struct{}
	// To control the max amount of concurrent tails
	tailLimiterCh chan struct{}
	// To run scheduled tasks (if configured)
	sched *scheduler
}

// New returns a new server.
func New() *Server {
	logger.Info("Creating server", version.String())

	s := Server{
		sshServerConfig: &gossh.ServerConfig{},
		catLimiterCh:    make(chan struct{}, config.Server.MaxConcurrentCats),
		tailLimiterCh:   make(chan struct{}, config.Server.MaxConcurrentTails),
		sched:           newScheduler(),
	}

	s.sshServerConfig.PasswordCallback = s.backgroundUserCallback
	s.sshServerConfig.PublicKeyCallback = server.PublicKeyCallback

	private, err := gossh.ParsePrivateKey(server.PrivateHostKey())
	if err != nil {
		logger.FatalExit(err)
	}
	s.sshServerConfig.AddHostKey(private)

	return &s
}

// Start the server.
func (s *Server) Start(ctx context.Context) int {
	logger.Info("Starting server")

	bindAt := fmt.Sprintf("%s:%d", config.Server.SSHBindAddress, config.Common.SSHPort)
	logger.Info("Binding server", bindAt)
	listener, err := net.Listen("tcp", bindAt)
	if err != nil {
		logger.FatalExit("Failed to open listening TCP socket", err)
	}

	go s.stats.start(ctx)
	go s.sched.start(ctx)

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

		go s.handleConnection(ctx, conn)
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	logger.Info("Handling connection")

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.sshServerConfig)
	if err != nil {
		logger.Error("Something just happened", err)
		return
	}

	s.stats.incrementConnections()

	go gossh.DiscardRequests(reqs)
	for newChannel := range chans {
		go s.handleChannel(ctx, sshConn, newChannel)
	}
}

func (s *Server) handleChannel(ctx context.Context, sshConn gossh.Conn, newChannel gossh.NewChannel) {
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

	if err := s.handleRequests(ctx, sshConn, requests, channel, user); err != nil {
		logger.Error(user, err)
		sshConn.Close()
	}
}

func (s *Server) handleRequests(ctx context.Context, sshConn gossh.Conn, in <-chan *gossh.Request, channel gossh.Channel, user *user.User) error {
	logger.Info(user, "Invoking request handler")

	for req := range in {
		var payload = struct{ Value string }{}
		gossh.Unmarshal(req.Payload, &payload)

		switch req.Type {
		case "shell":
			handlerCtx, cancel := context.WithCancel(ctx)

			var handler handlers.Handler
			var done <-chan struct{}

			switch user.Name {
			case config.ControlUser:
				handler, done = handlers.NewControlHandler(handlerCtx, user)
			default:
				handler, done = handlers.NewServerHandler(handlerCtx, user, s.catLimiterCh, s.tailLimiterCh)
			}

			go func() {
				// Handler finished work, cancel all remaining routines
				defer cancel()
				<-done
			}()

			go func() {
				// Broken pipe, cancel
				defer cancel()

				io.Copy(channel, handler)
			}()

			go func() {
				// Broken pipe, cancel
				defer cancel()

				io.Copy(handler, channel)
			}()

			go func() {
				defer cancel()

				if err := sshConn.Wait(); err != nil && err != io.EOF {
					logger.Error(user, err)
				}
				s.stats.decrementConnections()
				logger.Info(user, "Good bye Mister!")
			}()

			go func() {
				<-handlerCtx.Done()
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

func (s *Server) backgroundUserCallback(c gossh.ConnMetadata, authPayload []byte) (*gossh.Permissions, error) {
	user := user.New(c.User(), c.RemoteAddr().String())
	authInfo := string(authPayload)

	if user.Name == config.ControlUser && authInfo == config.ControlUser {
		logger.Debug(user, "Granting permissions to control user")
		return nil, nil
	}

	if user.Name == config.ScheduleUser && s.schedueleUserCanHaveSSHSession(c.RemoteAddr().String(), user, authInfo) {
		logger.Debug(user, "Granting SSH connection to schedule user")
		return nil, nil
	}

	return nil, fmt.Errorf("user %s not authorized", user)
}

func (s *Server) schedueleUserCanHaveSSHSession(addr string, user *user.User, jobName string) bool {
	logger.Debug("schedueleUserCanHaveSSHSession", user, jobName)
	splitted := strings.Split(addr, ":")
	ip := splitted[0]

	for _, job := range config.Server.Schedule {
		if job.Name != jobName {
			continue
		}
		for _, myAddr := range job.AllowFrom {
			myIPs, err := net.LookupIP(myAddr)
			if err != nil {
				logger.Error(user, myAddr, err)
				continue
			}

			for _, myIP := range myIPs {
				logger.Debug("schedueleUserCanHaveSSHSession", "Comparing IP addresses", ip, myIP.String())
				if ip == myIP.String() {
					return true
				}
			}
		}
	}

	return false
}
