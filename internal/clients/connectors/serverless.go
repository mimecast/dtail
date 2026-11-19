package connectors

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

// ServerlessHandlerFactory creates the in-process server-side handler used by serverless mode.
type ServerlessHandlerFactory interface {
	NewServerlessHandler(context.Context, string) (sessionHandlers.Handler, error)
}

type contextGracefulServerlessHandler interface {
	GracefulShutdownContext(context.Context)
}

type outputReaderAttacher interface {
	AttachOutputReader()
}

// Serverless creates a server object directly without TCP.
type Serverless struct {
	handler        handlers.Handler
	commands       []string
	sessionSpec    sessionspec.Spec
	sessionState   committedSessionState
	interactive    bool
	userName       string
	handlerFactory ServerlessHandlerFactory
	logger         logging.Logger
}

var _ Connector = (*Serverless)(nil)

// NewServerless starts a new serverless session.
func NewServerless(userName string, handler handlers.Handler,
	commands []string, sessionSpec sessionspec.Spec, interactive bool,
	handlerFactory ServerlessHandlerFactory, logger logging.Logger) *Serverless {

	logger = logging.OrNop(logger)
	logger.Debug("Creating new serverless connector", handler, commands)
	return &Serverless{
		userName:       userName,
		handler:        handler,
		commands:       commands,
		sessionSpec:    sessionSpec,
		interactive:    interactive,
		handlerFactory: handlerFactory,
		logger:         logger,
	}
}

// Server returns serverless server indicator.
func (s *Serverless) Server() string {
	return "local(serverless)"
}

// Handler returns the handler used for the serverless connection.
func (s *Serverless) Handler() handlers.Handler {
	return s.handler
}

// SupportsQueryUpdates reports whether the in-process server advertised
// runtime query update support to the client handler.
func (s *Serverless) SupportsQueryUpdates(timeout time.Duration) bool {
	return supportsQueryUpdates(s.handler, timeout)
}

// ApplySessionSpec starts or updates the in-process interactive session state.
func (s *Serverless) ApplySessionSpec(spec sessionspec.Spec, timeout time.Duration) error {
	return applySessionSpec(s.Server(), s.handler, &s.sessionState, spec, timeout, s.logger)
}

// ApplySessionSpecWithGeneration starts or updates the in-process interactive
// session state using an explicit committed generation as the update base.
func (s *Serverless) ApplySessionSpecWithGeneration(spec sessionspec.Spec, generation uint64, timeout time.Duration) error {
	return applySessionSpecWithGeneration(s.Server(), s.handler, &s.sessionState, spec, generation, false, timeout, s.logger)
}

// CommittedSession returns the last server-acknowledged session state.
func (s *Serverless) CommittedSession() (sessionspec.Spec, uint64, bool) {
	return s.sessionState.snapshot()
}

// RestoreCommittedSession resets the local session snapshot without advancing
// the generation.
func (s *Serverless) RestoreCommittedSession(spec sessionspec.Spec, generation uint64, committed bool) {
	s.sessionState.restore(spec, generation, committed)
}

// Start the serverless connection.
func (s *Serverless) Start(ctx context.Context, cancel context.CancelFunc,
	throttleCh, statsCh chan struct{}) {

	s.logger.Debug("Starting serverless connector")
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		if err := s.handle(ctx, cancel); err != nil {
			if shouldReportConnectionError(ctx, err) {
				s.handler.ReportServerError("serverless session failed: " + err.Error())
			}
		}
	}()
	<-ctx.Done()
	<-done
}

func (s *Serverless) handle(ctx context.Context, cancel context.CancelFunc) error {
	s.logger.Debug("Creating server handler for a serverless session")

	if s.handlerFactory == nil {
		return io.ErrClosedPipe
	}
	// Keep the in-process server lifetime independent from client work
	// cancellation until GracefulShutdownContext has claimed final aggregate
	// output. Output failure cancels this context immediately and selects abrupt
	// shutdown below.
	outputDrainCtx, cancelOutputDrain := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelOutputDrain()
	serverHandler, err := s.handlerFactory.NewServerlessHandler(outputDrainCtx, s.userName)
	if err != nil {
		return err
	}
	// Publish reader ownership before any I/O goroutine or command dispatch can
	// run. A fast serverless command may otherwise reach shutdown before the
	// server reader's first Read and skip its delivery barrier as though a raw
	// channel consumer owned the output.
	if outputReader, ok := serverHandler.(outputReaderAttacher); ok {
		outputReader.AttachOutputReader()
	}
	// Use buffered channels to prevent deadlock
	// This approach avoids the circular dependency of direct io.Copy

	// Channels for data flow
	toServer := make(chan []byte, 100)
	fromServer := make(chan []byte, 100)

	// Error tracking
	errChan := make(chan error, 4)
	clientOutputErr := make(chan error, 1)
	var ioWg sync.WaitGroup

	// Read from client handler
	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		defer close(toServer)
		buf := make([]byte, 32*1024)
		for {
			n, readErr := s.handler.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case toServer <- data:
				case <-ctx.Done():
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					errChan <- readErr
				}
				return
			}
		}
	}()

	// Write to server handler
	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		for data := range toServer {
			if _, writeErr := serverHandler.Write(data); writeErr != nil {
				errChan <- writeErr
				return
			}
		}
	}()

	// Read from server handler
	// Cancellation stops new work but must not discard output that the server
	// produces while shutting down. Only abort this reader if the client output
	// writer itself has stopped accepting data.
	clientOutputStopped := make(chan struct{})
	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		defer close(fromServer)
		buf := make([]byte, 64*1024) // Larger buffer for server responses
		for {
			n, readErr := serverHandler.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case fromServer <- data:
				case <-clientOutputStopped:
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					errChan <- readErr
				}
				return
			}
		}
	}()

	// Write to client handler
	serverOutputDone := make(chan struct{})
	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		defer close(serverOutputDone)
		defer close(clientOutputStopped)
		for data := range fromServer {
			n, writeErr := s.handler.Write(data)
			if writeErr == nil && n != len(data) {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				cancelOutputDrain()
				clientOutputErr <- writeErr
				return
			}
		}
	}()

	dispatchErr := dispatchInitialCommands(s.Server(), s.handler, s.commands, s.interactive,
		s.sessionSpec, &s.sessionState, s.logger)
	var transferErr error
	if dispatchErr == nil {
		select {
		case <-s.handler.Done():
			s.logger.Trace("<-s.handler.Done()")
			// The client handler marks itself done as soon as it receives the
			// hidden close message. Keep the in-process server alive long enough
			// for the remaining output and close ACK to drain instead of canceling
			// the whole session immediately.
			select {
			case <-serverOutputDone:
				s.logger.Trace("Server transfer done after client close")
			case <-ctx.Done():
				s.logger.Trace("<-ctx.Done() while waiting for server transfer")
			case <-time.After(6 * time.Second):
				s.logger.Debug("Timed out waiting for server transfer after client close")
			}
		case <-serverOutputDone:
			s.logger.Trace("Server transfer done")
		case <-ctx.Done():
			s.logger.Trace("<-ctx.Done()")
		case transferErr = <-errChan:
			s.logger.Trace("Serverless transfer failed", transferErr)
		case transferErr = <-clientOutputErr:
			s.logger.Trace("Serverless client output failed", transferErr)
		}
	}

	// Stop the server first so its output reader reaches EOF, then drain all
	// output already produced during shutdown before finalizing the client
	// handler. In particular, MaprHandler.Shutdown performs its final aggregate
	// flush, so it must run after the last server-to-client Write has completed.
	s.logger.Debug("Terminating serverless connection")
	if outputDrainCtx.Err() != nil {
		serverHandler.Shutdown()
	} else if gracefulHandler, ok := serverHandler.(contextGracefulServerlessHandler); ok {
		gracefulHandler.GracefulShutdownContext(outputDrainCtx)
	} else {
		serverHandler.Shutdown()
	}
	cancel()
	<-serverOutputDone
	s.handler.Shutdown()
	ioWg.Wait()

	if dispatchErr != nil {
		return dispatchErr
	}
	if transferErr != nil {
		return transferErr
	}
	select {
	case outputErr := <-clientOutputErr:
		return outputErr
	default:
	}
	select {
	case pendingErr := <-errChan:
		return pendingErr
	default:
	}

	return nil
}
