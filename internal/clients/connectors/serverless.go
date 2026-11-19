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

type serverlessIO struct {
	toServer            chan []byte
	fromServer          chan []byte
	errors              chan error
	clientOutputErrors  chan error
	clientOutputStopped chan struct{}
	serverOutputDone    chan struct{}
	wg                  sync.WaitGroup
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
	releaseThrottle := func() {}
	if throttleCh != nil {
		s.logger.Debug(s.Server(), "Throttling connection", len(throttleCh), cap(throttleCh))
		select {
		case throttleCh <- struct{}{}:
		case <-ctx.Done():
			s.logger.Debug(s.Server(), "Not establishing connection as context is done",
				len(throttleCh), cap(throttleCh))
			return
		}

		var throttleReleased sync.Once
		releaseThrottle = func() {
			throttleReleased.Do(func() {
				s.logger.Debug(s.Server(), "Unthrottling connection", len(throttleCh), cap(throttleCh))
				<-throttleCh
			})
		}
		defer releaseThrottle()
	}

	if statsCh != nil {
		s.logger.Debug(s.Server(), "Incrementing connection stats")
		select {
		case statsCh <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() {
			s.logger.Debug(s.Server(), "Decrementing connection stats")
			<-statsCh
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		if err := s.handleConnection(ctx, cancel, releaseThrottle); err != nil {
			if shouldReportConnectionError(ctx, err) {
				s.handler.ReportServerError("serverless session failed: " + err.Error())
			}
		}
	}()
	<-ctx.Done()
	<-done
}

func (s *Serverless) handle(ctx context.Context, cancel context.CancelFunc) error {
	return s.handleConnection(ctx, cancel, func() {})
}

func (s *Serverless) handleConnection(ctx context.Context, cancel context.CancelFunc,
	connectionEstablished func()) error {
	s.logger.Debug("Creating server handler for a serverless session")
	// Keep the in-process server lifetime independent from client work
	// cancellation until GracefulShutdownContext has claimed final aggregate
	// output. Output failure cancels this context immediately and selects abrupt
	// shutdown below.
	outputDrainCtx, cancelOutputDrain := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelOutputDrain()
	serverHandler, err := s.newServerlessHandler(outputDrainCtx)
	if err != nil {
		return err
	}
	transfer := s.startServerlessIO(ctx, serverHandler, cancelOutputDrain)
	dispatchErr := dispatchInitialCommands(s.Server(), s.handler, s.commands, s.interactive,
		s.sessionSpec, &s.sessionState, s.logger)
	var transferErr error
	if dispatchErr == nil {
		connectionEstablished()
		transferErr = s.waitForServerlessIO(ctx, transfer)
	}

	s.shutdownServerlessIO(outputDrainCtx, cancel, serverHandler, transfer)
	return serverlessResult(dispatchErr, transferErr, transfer)
}

func (s *Serverless) newServerlessHandler(ctx context.Context) (sessionHandlers.Handler, error) {
	if s.handlerFactory == nil {
		return nil, io.ErrClosedPipe
	}
	serverHandler, err := s.handlerFactory.NewServerlessHandler(ctx, s.userName)
	if err != nil {
		return nil, err
	}
	// Publish reader ownership before any I/O goroutine or command dispatch can
	// run. A fast serverless command may otherwise reach shutdown before the
	// server reader's first Read and skip its delivery barrier as though a raw
	// channel consumer owned the output.
	if outputReader, ok := serverHandler.(outputReaderAttacher); ok {
		outputReader.AttachOutputReader()
	}
	return serverHandler, nil
}

func (s *Serverless) startServerlessIO(ctx context.Context, serverHandler sessionHandlers.Handler,
	cancelOutputDrain context.CancelFunc) *serverlessIO {
	// Use buffered channels to prevent deadlock
	// This approach avoids the circular dependency of direct io.Copy
	transfer := &serverlessIO{
		toServer:            make(chan []byte, 100),
		fromServer:          make(chan []byte, 100),
		errors:              make(chan error, 4),
		clientOutputErrors:  make(chan error, 1),
		clientOutputStopped: make(chan struct{}),
		serverOutputDone:    make(chan struct{}),
	}
	transfer.wg.Add(4)
	go s.copyClientToQueue(ctx, transfer)
	go s.copyQueueToServer(serverHandler, transfer)
	go s.copyServerToQueue(serverHandler, transfer)
	go s.copyQueueToClient(cancelOutputDrain, transfer)
	return transfer
}

func (s *Serverless) copyClientToQueue(ctx context.Context, transfer *serverlessIO) {
	defer transfer.wg.Done()
	defer close(transfer.toServer)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := s.handler.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			select {
			case transfer.toServer <- data:
			case <-ctx.Done():
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				transfer.errors <- readErr
			}
			return
		}
	}
}

func (s *Serverless) copyQueueToServer(serverHandler sessionHandlers.Handler, transfer *serverlessIO) {
	defer transfer.wg.Done()
	for data := range transfer.toServer {
		if _, err := serverHandler.Write(data); err != nil {
			transfer.errors <- err
			return
		}
	}
}

func (s *Serverless) copyServerToQueue(serverHandler sessionHandlers.Handler, transfer *serverlessIO) {
	defer transfer.wg.Done()
	defer close(transfer.fromServer)
	buf := make([]byte, 64*1024)
	for {
		n, readErr := serverHandler.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			select {
			case transfer.fromServer <- data:
			case <-transfer.clientOutputStopped:
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				transfer.errors <- readErr
			}
			return
		}
	}
}

func (s *Serverless) copyQueueToClient(cancelOutputDrain context.CancelFunc, transfer *serverlessIO) {
	defer transfer.wg.Done()
	defer close(transfer.serverOutputDone)
	defer close(transfer.clientOutputStopped)
	for data := range transfer.fromServer {
		n, writeErr := s.handler.Write(data)
		if writeErr == nil && n != len(data) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			cancelOutputDrain()
			transfer.clientOutputErrors <- writeErr
			return
		}
	}
}

func (s *Serverless) waitForServerlessIO(ctx context.Context, transfer *serverlessIO) error {
	select {
	case <-s.handler.Done():
		s.logger.Trace("<-s.handler.Done()")
		s.waitForServerOutput(ctx, transfer.serverOutputDone)
	case <-transfer.serverOutputDone:
		s.logger.Trace("Server transfer done")
	case <-ctx.Done():
		s.logger.Trace("<-ctx.Done()")
	case err := <-transfer.errors:
		s.logger.Trace("Serverless transfer failed", err)
		return err
	case err := <-transfer.clientOutputErrors:
		s.logger.Trace("Serverless client output failed", err)
		return err
	}
	return nil
}

func (s *Serverless) waitForServerOutput(ctx context.Context, serverOutputDone <-chan struct{}) {
	// The client handler marks itself done as soon as it receives the hidden
	// close message. Keep the server alive for remaining output and close ACK.
	select {
	case <-serverOutputDone:
		s.logger.Trace("Server transfer done after client close")
	case <-ctx.Done():
		s.logger.Trace("<-ctx.Done() while waiting for server transfer")
	case <-time.After(6 * time.Second):
		s.logger.Debug("Timed out waiting for server transfer after client close")
	}
}

func (s *Serverless) shutdownServerlessIO(outputDrainCtx context.Context, cancel context.CancelFunc,
	serverHandler sessionHandlers.Handler, transfer *serverlessIO) {
	// Stop the server first so its output reader reaches EOF, then drain all
	// output already produced during shutdown before finalizing the client
	// handler. MaprHandler.Shutdown performs its final aggregate flush after the
	// last server-to-client Write has completed.
	s.logger.Debug("Terminating serverless connection")
	if outputDrainCtx.Err() != nil {
		serverHandler.Shutdown()
	} else if gracefulHandler, ok := serverHandler.(contextGracefulServerlessHandler); ok {
		gracefulHandler.GracefulShutdownContext(outputDrainCtx)
	} else {
		serverHandler.Shutdown()
	}
	cancel()
	<-transfer.serverOutputDone
	s.handler.Shutdown()
	transfer.wg.Wait()
}

func serverlessResult(dispatchErr, transferErr error, transfer *serverlessIO) error {
	if dispatchErr != nil {
		return dispatchErr
	}
	if transferErr != nil {
		return transferErr
	}
	select {
	case outputErr := <-transfer.clientOutputErrors:
		return outputErr
	default:
	}
	select {
	case pendingErr := <-transfer.errors:
		return pendingErr
	default:
	}
	return nil
}
