package connectors

import (
	"context"
	"io"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	serverHandlers "github.com/mimecast/dtail/internal/server/handlers"
	user "github.com/mimecast/dtail/internal/user/server"
)

// Serverless creates a server object directly without TCP.
type Serverless struct {
	handler  handlers.Handler
	commands []string
	userName string
}

// NewServerConnection returns a new connection.
func NewServerless(userName string, handler handlers.Handler, commands []string) *Serverless {
	dlog.Client.Debug("Creating new serverless connector", handler, commands)
	return &Serverless{
		userName: userName,
		handler:  handler,
		commands: commands,
	}
}

func (s *Serverless) Server() string {
	return "local(serverless)"
}

func (s *Serverless) Handler() handlers.Handler {
	return s.handler
}

func (s *Serverless) Start(ctx context.Context, cancel context.CancelFunc, throttleCh, statsCh chan struct{}) {
	go func() {
		defer cancel()

		if err := s.handle(ctx, cancel); err != nil {
			dlog.Client.Warn(err)
		}
	}()

	<-ctx.Done()
}

func (s *Serverless) handle(ctx context.Context, cancel context.CancelFunc) error {
	dlog.Client.Debug("Creating server handler for a serverless session")

	user, err := user.New(s.userName, s.Server())
	if err != nil {
		return err
	}

	var serverHandler serverHandlers.Handler
	switch s.userName {
	case config.ControlUser:
		serverHandler = serverHandlers.NewControlHandler(user)
	default:
		serverHandler = serverHandlers.NewServerHandler(
			user,
			make(chan struct{}, config.Server.MaxConcurrentCats),
			make(chan struct{}, config.Server.MaxConcurrentTails),
		)
	}

	terminate := func() {
		serverHandler.Shutdown()
		cancel()
	}

	go func() {
		io.Copy(serverHandler, s.handler)
		terminate()
	}()

	go func() {
		io.Copy(s.handler, serverHandler)
		terminate()
	}()

	go func() {
		select {
		case <-s.handler.Done():
		case <-ctx.Done():
		}
		terminate()
	}()

	// Send all commands to client.
	for _, command := range s.commands {
		dlog.Client.Debug(command)
		s.handler.SendMessage(command)
	}

	<-ctx.Done()
	s.handler.Shutdown()

	return nil
}
