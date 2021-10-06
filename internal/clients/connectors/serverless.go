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
	dlog.Client.Debug("Starting serverless connector")
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
	case config.HealthUser:
		dlog.Client.Debug("Creating serverless health handler")
		serverHandler = serverHandlers.NewHealthHandler(user)
	default:
		dlog.Client.Debug("Creating serverless server handler")
		serverHandler = serverHandlers.NewServerHandler(
			user,
			make(chan struct{}, config.Server.MaxConcurrentCats),
			make(chan struct{}, config.Server.MaxConcurrentTails),
		)
	}

	terminate := func() {
		dlog.Client.Debug("Terminating serverless connection")
		serverHandler.Shutdown()
		cancel()
	}

	go func() {
		io.Copy(serverHandler, s.handler)
		dlog.Client.Trace("io.Copy(serverHandler, s.handler) => done")
		terminate()
	}()

	go func() {
		io.Copy(s.handler, serverHandler)
		dlog.Client.Trace("io.Copy(s.handler, serverHandler) => done")
		terminate()
	}()

	go func() {
		select {
		case <-s.handler.Done():
			dlog.Client.Trace("<-s.handler.Done()")
		case <-ctx.Done():
			dlog.Client.Trace("<-ctx.Done()")
		}
		terminate()
	}()

	// Send all commands to client.
	for _, command := range s.commands {
		dlog.Client.Debug("Sending command to serverless server", command)
		s.handler.SendMessage(command)
	}

	<-ctx.Done()
	dlog.Client.Trace("s.handler.Shutdown()")
	s.handler.Shutdown()

	return nil
}
