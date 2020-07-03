package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/server/background"
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
	catLimiter chan struct{}
	// To control the max amount of concurrent tails
	tailLimiter chan struct{}
	// To run scheduled tasks (if configured)
	sched *scheduler
	// Mointor log files for pattern (if configured)
	cont *continuous
	// Wait counter, e.g. there might be still subprocesses (forked by drun) to be killed.
	shutdownWaitFor chan struct{}
	// Background jobs
	background background.Background
}

// New returns a new server.
func New() *Server {
	logger.Info("Creating server", version.String())

	s := Server{
		sshServerConfig: &gossh.ServerConfig{},
		catLimiter:      make(chan struct{}, config.Server.MaxConcurrentCats),
		tailLimiter:     make(chan struct{}, config.Server.MaxConcurrentTails),
		shutdownWaitFor: make(chan struct{}, 1000),
		sched:           newScheduler(),
		cont:            newContinuous(),
		background:      background.New(),
	}

	s.sshServerConfig.PasswordCallback = s.Callback
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
	go s.cont.start(ctx)
	go s.listenerLoop(ctx, listener)

	select {
	case <-ctx.Done():
		// Wait until all commands/jobs/children are no more!
		s.wait()
	}

	// For future use.
	return 0
}

func (s *Server) wait() {
	for {
		num := len(s.shutdownWaitFor)
		logger.Debug("Waiting for stuff to finish", num)
		if num <= 0 {
			return
		}
		time.Sleep(time.Second)
	}
}

func (s *Server) listenerLoop(ctx context.Context, listener net.Listener) {
	logger.Debug("Starting listener loop")

	for {
		conn, err := listener.Accept() // Blocking
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
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
				handler, done = handlers.NewServerHandler(handlerCtx, ctx, user, s.catLimiter, s.tailLimiter, s.shutdownWaitFor, s.background)
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

// Callback for SSH authentication.
func (s *Server) Callback(c gossh.ConnMetadata, authPayload []byte) (*gossh.Permissions, error) {
	user := user.New(c.User(), c.RemoteAddr().String())
	authInfo := string(authPayload)

	splitted := strings.Split(c.RemoteAddr().String(), ":")
	remoteIP := splitted[0]

	switch user.Name {
	case config.ControlUser:
		if authInfo == config.ControlUser {
			logger.Debug(user, "Granting permissions to control user")
			return nil, nil
		}
	case config.ScheduleUser:
		for _, job := range config.Server.Schedule {
			if s.backgroundCanSSH(user, authInfo, remoteIP, job.Name, job.AllowFrom) {
				logger.Debug(user, "Granting SSH connection")
				return nil, nil
			}
		}
	case config.ContinuousUser:
		for _, job := range config.Server.Continuous {
			if s.backgroundCanSSH(user, authInfo, remoteIP, job.Name, job.AllowFrom) {
				logger.Debug(user, "Granting SSH connection")
				return nil, nil
			}
		}
	default:
	}

	return nil, fmt.Errorf("user %s not authorized", user)
}

func (s *Server) backgroundCanSSH(user *user.User, jobName, remoteIP, allowedJobName string, allowFrom []string) bool {
	logger.Debug("backgroundCanSSH", user, jobName, remoteIP, allowedJobName, allowFrom)

	if jobName != allowedJobName {
		logger.Debug(user, jobName, "backgroundCanSSH", "Job name does not match, skipping to next one...", allowedJobName)
		return false
	}

	for _, myAddr := range allowFrom {
		ips, err := net.LookupIP(myAddr)
		if err != nil {
			logger.Debug(user, jobName, "backgroundCanSSH", "Unable to lookup IP address for allowed hosts lookup, skipping to next one...", myAddr, err)
			continue
		}

		for _, ip := range ips {
			logger.Debug(user, jobName, "backgroundCanSSH", "Comparing IP addresses", remoteIP, ip.String())
			if remoteIP == ip.String() {
				return true
			}
		}
	}

	return false
}
