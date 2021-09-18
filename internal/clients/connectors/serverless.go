package connectors

import (
	"context"
	"io"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
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
	s := Serverless{
		userName: userName,
		handler:  handler,
		commands: commands,
	}

	logger.Debug("Creating new serverless connector", handler, commands)
	return &s
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
			logger.Warn(err)
		}
	}()

	<-ctx.Done()
}

func (s *Serverless) handle(ctx context.Context, cancel context.CancelFunc) error {
	logger.Debug("Creating server handler for a serverless session")

	serverHandler := serverHandlers.NewServerHandler(
		user.New(s.userName, s.Server()),
		make(chan struct{}, config.Server.MaxConcurrentCats),
		make(chan struct{}, config.Server.MaxConcurrentTails),
	)

	go func() {
		io.Copy(serverHandler, s.handler)
		cancel()
	}()

	go func() {
		io.Copy(s.handler, serverHandler)
		cancel()
	}()

	go func() {
		select {
		case <-s.handler.Done():
		case <-ctx.Done():
		}
		cancel()
	}()

	// Send all commands to client.
	for _, command := range s.commands {
		logger.Debug(command)
		s.handler.SendMessage(command)
	}

	<-ctx.Done()
	s.handler.Shutdown()

	return nil
}
