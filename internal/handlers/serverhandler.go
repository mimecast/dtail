package handlers

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/sessionuser"

	gossh "golang.org/x/crypto/ssh"
)

// ServerHandler implements the Reader and Writer interfaces to handle
// the Bi-directional communication between SSH client and server.
// This handler implements the handler of the SSH server.
type ServerHandler struct {
	*baseHandler
	catLimiter          chan struct{}
	tailLimiter         chan struct{}
	serverCfg           *config.ServerConfig
	readTimings         readTimings
	authKeyStore        *authkey.Store
	colorizer           Colorizer
	regex               string
	commands            map[string]commandHandler
	sessionState        sessionCommandState
	commandBatch        commandBatch
	idleShutdownStarted atomic.Bool
	// Track pending files waiting for limiter slots
	pendingFiles int32
}

type commandHandler func(context.Context, lcontext.LContext, int, []string, func())

// HandlerLoggers contains the logger roles used by a server handler. They are
// distinct in serverless mode, where file-reader diagnostics belong to the
// client/common logger while protocol diagnostics retain the server logger.
type HandlerLoggers struct {
	Diagnostics logging.Logger
	Reader      logging.Logger
}

func (l HandlerLoggers) normalized() HandlerLoggers {
	l.Diagnostics = logging.OrNop(l.Diagnostics)
	l.Reader = logging.OrNop(l.Reader)
	return l
}

var _ Handler = (*ServerHandler)(nil)

// NewServerHandler returns the server handler.
func NewServerHandler(ctx context.Context, user *user.User, dependencies Dependencies) (*ServerHandler, error) {

	serverCfg := dependencies.ServerConfig
	loggers := dependencies.Loggers
	loggers = loggers.normalized()
	loggers.Diagnostics.Debug(user, "Creating new server handler")
	if user == nil {
		return nil, fmt.Errorf("create server handler: user must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("create server handler: context must not be nil")
	}
	if serverCfg == nil {
		return nil, fmt.Errorf("create server handler: server config must not be nil")
	}

	if dependencies.AuthKeyStore == nil {
		return nil, fmt.Errorf("create server handler: auth-key store must not be nil")
	}
	if err := validateOutputBufferConfig(serverCfg); err != nil {
		return nil, err
	}

	h := ServerHandler{
		baseHandler: newBaseHandler(ctx, baseHandlerConfig{
			logger:              loggers.Diagnostics,
			readerLogger:        loggers.Reader,
			serverlessOutput:    dependencies.ServerlessOutput,
			user:                user,
			maxCommandFrameSize: serverCfg.MaxCommandFrameSize,
		}),
		catLimiter:   dependencies.CatLimiter,
		tailLimiter:  dependencies.TailLimiter,
		serverCfg:    serverCfg,
		readTimings:  newReadTimings(serverCfg),
		authKeyStore: dependencies.AuthKeyStore,
		colorizer:    dependencies.Colorizer,
		regex:        ".",
	}
	h.handleCommandCb = h.handleUserCommand
	h.prepareCommandContextCb = h.prepareCommandContext
	h.commands = h.newCommandRegistry()
	h.output.configure(h.outputManagerConfig(), loggers.Diagnostics)
	h.activeGeneration = h.sessionState.currentGeneration

	s := strings.Split(dependencies.Hostname, ".")
	h.hostname = s[0]
	h.send(h.serverMessages, protocol.HiddenCapabilitiesPrefix+strings.Join(dependencies.Capabilities, " "))

	return &h, nil
}

// validateOutputBufferConfig prevents a valid maximum-length line from being
// rejected after protocol framing. A flush can contain the tail of the normal
// 64 KiB writer batch plus that line, so reserve another full batch as bounded
// framing/path overhead. Zero values use the safe defaults.
func validateOutputBufferConfig(serverCfg *config.ServerConfig) error {
	maxLineLength := positiveIntOrDefault(serverCfg.MaxLineLength, 1024*1024)
	bufferMaxBytes := positiveIntOrDefault(serverCfg.OutputBufferMaxBytes, defaultOutputBufferMaxBytes)
	minimum := maxLineLength + 2*networkWriterBufferSize
	if bufferMaxBytes < minimum {
		return fmt.Errorf("create server handler: OutputBufferMaxBytes %d must be at least %d for MaxLineLength %d",
			bufferMaxBytes, minimum, maxLineLength)
	}
	return nil
}

func (h *ServerHandler) handleUserCommand(ctx context.Context, ltx lcontext.LContext,
	argc int, args []string, commandName string) {

	h.Logger().Debug(h.user, "Handling user command", argc, args)
	// The close acknowledgement completes an existing shutdown handshake; it
	// is protocol control traffic rather than new workload. Process it even
	// after graceful shutdown has sealed command admission, and keep it out of
	// commandWg so shutdown never waits on its own acknowledgement.
	if strings.EqualFold(commandName, ".ack") {
		if isInputBatchBegin(argc, args) {
			h.BeginCommandBatch()
			cancelCommandContext(ctx)
			return
		}
		if isInputBatchComplete(argc, args) {
			h.completeCommandBatch(h.connCtx)
			cancelCommandContext(ctx)
			return
		}
		h.handleAckCommand(argc, args)
		cancelCommandContext(ctx)
		return
	}
	shutdownOnCompletion := shouldShutdownOnCommandCompletion(commandName)
	if !h.beginCommand(hasSessionCommandAdmission(ctx)) {
		cancelCommandContext(ctx)
		return
	}
	markCommandAdmitted(ctx)
	if strings.EqualFold(commandName, "SESSION") {
		ctx = withSessionCommandAdmission(ctx)
	}
	var commandFinishOnce sync.Once
	commandFinished := func() {
		commandFinishOnce.Do(func() {
			defer h.finishCommand()
			activeCommands := h.decrementActiveCommands()
			pendingFiles := atomic.LoadInt32(&h.pendingFiles)
			h.Logger().Debug(h.user, "Command finished", "activeCommands", activeCommands, "pendingFiles", pendingFiles)

			// Only shutdown if no active commands AND no pending files.
			// AUTHKEY is a session-side effect command and should not terminate the shell
			// because user commands may still follow in the same session.
			if shutdownOnCompletion && activeCommands == 0 && pendingFiles == 0 &&
				!h.sessionState.keepAlive() && !h.isStopping() {
				h.triggerIdleShutdown(ctx)
			}

			// Release this command's child context after any final output has
			// drained. The handler's connection-derived root handles session-wide
			// cancellation directly, without a watcher goroutine per command.
			cancelCommandContext(ctx)
		})
	}
	defer recoverHandlerPanic(h.Logger(), h.user, "command dispatch cleanup", h.abortAfterPanic)
	defer h.finishCommandInitialization()
	defer recoverHandlerPanic(h.Logger(), h.user, "command dispatch", func() {
		defer h.abortAfterPanic()
		commandFinished()
	})

	handler, found := h.commands[commandName]
	if !found {
		h.sendln(h.serverMessages, h.Logger().Error(h.user,
			"Received unknown user command", commandName, argc, args))
		commandFinished()
		return
	}

	handler(ctx, ltx, argc, args, commandFinished)
}

func isInputBatchBegin(argc int, args []string) bool {
	return argc == 3 && strings.EqualFold(args[1], "input") && strings.EqualFold(args[2], "begin")
}

func isInputBatchComplete(argc int, args []string) bool {
	return argc == 3 && strings.EqualFold(args[1], "input") && strings.EqualFold(args[2], "complete")
}

// BeginCommandBatch prevents a fast command from closing a session or
// finishing aggregate input before the remaining bounded batch is admitted.
func (h *ServerHandler) BeginCommandBatch() {
	h.commandBatch.begin()
}

func (h *ServerHandler) completeCommandBatch(ctx context.Context) {
	h.finishCommandBatch(ctx, &h.commandBatch)
}

func (h *ServerHandler) finishCommandBatch(ctx context.Context, batch *commandBatch) {
	for _, aggregate := range batch.complete() {
		aggregate.FinishInput()
	}
	pending, active := h.PendingAndActive()
	if pending == 0 && active == 0 && !h.sessionState.keepAlive() && !h.isStopping() {
		// The marker is processed by the sole client-to-server writer. Run the
		// close handshake separately so that writer remains free to deliver its
		// acknowledgement.
		go func() {
			defer recoverHandlerPanic(h.Logger(), h.user, "idle shutdown", h.abortAfterPanic)
			h.triggerIdleShutdown(ctx)
		}()
	}
}

func (h *ServerHandler) triggerIdleShutdown(ctx context.Context) {
	if h.commandBatch.isOpen() || !h.idleShutdownStarted.CompareAndSwap(false, true) {
		return
	}
	h.shutdown(ctx)
}

func shouldShutdownOnCommandCompletion(commandName string) bool {
	switch {
	case strings.EqualFold(commandName, "AUTHKEY"):
		return false
	case strings.EqualFold(commandName, "SESSION"):
		return false
	default:
		return true
	}
}

func (h *ServerHandler) prepareCommandContext(ctx context.Context, commandName string) (context.Context, func()) {
	var mode omode.Mode
	switch commandName {
	case "cat":
		mode = omode.CatClient
	case "grep":
		mode = omode.GrepClient
	case "tail":
		mode = omode.TailClient
	default:
		return ctx, nil
	}
	admission, ok := ctx.Value(commandAdmissionResultKey).(*commandAdmissionResult)
	if !ok {
		admission = &commandAdmissionResult{}
		ctx = context.WithValue(ctx, commandAdmissionResultKey, admission)
	}
	reservation := newPendingInputReservation(ctx, h, mode)
	reservation.admission = admission
	batch := commandBatchFromContext(ctx)
	if batch == nil {
		batch = &h.commandBatch
	}
	reservation.inputBatch = batch
	reservation.inputBatchRead = batch.beginRead(mode, reservation.aggregate)
	reservation.shutdownCoordinator.inputBatchOwned = reservation.inputBatchRead.tracked
	return withPendingInputReservation(ctx, reservation), reservation.releaseIfUnclaimed
}

func (h *ServerHandler) newCommandRegistry() map[string]commandHandler {
	return map[string]commandHandler{
		"grep":    h.makeReadCommandHandler(omode.GrepClient, 1),
		"cat":     h.makeReadCommandHandler(omode.CatClient, 1),
		"tail":    h.makeReadCommandHandler(omode.TailClient, 10),
		"map":     h.handleMapCommand,
		".ack":    h.handleAckUserCommand,
		"AUTHKEY": h.handleAuthKeyCommand,
		"SESSION": h.handleSessionCommand,
		"authkey": h.handleAuthKeyCommand,
		"session": h.handleSessionCommand,
	}
}

func (h *ServerHandler) makeReadCommandHandler(mode omode.Mode, tailBackoff int) commandHandler {
	return func(ctx context.Context, ltx lcontext.LContext, argc int, args []string, commandFinished func()) {
		reservation := pendingInputReservationFromContext(ctx)
		var command *readCommand
		if reservation != nil {
			command = newReadCommandWithAggregate(h, mode, reservation.aggregate)
		} else {
			command = newReadCommand(h, mode)
		}
		command.adoptPendingInputReservation(reservation)
		if reservation == nil {
			batch := commandBatchFromContext(ctx)
			if batch == nil {
				batch = &h.commandBatch
			}
			command.inputBatch = batch
			command.inputBatchRead = batch.beginRead(mode, command.aggregate)
			command.shutdownCoordinator.inputBatchOwned = command.inputBatchRead.tracked
		}
		go func() {
			defer recoverHandlerPanic(h.Logger(), h.user, "read command cleanup", h.abortAfterPanic)
			defer commandFinished()
			defer command.completeInputBatch()
			defer recoverHandlerPanic(h.Logger(), h.user, "read command", h.abortAfterPanic)
			command.Start(ctx, ltx, argc, args, tailBackoff)
		}()
	}
}

func (h *ServerHandler) handleMapCommand(ctx context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
	command, aggregate, err := newMapCommand(h, args)
	if err != nil {
		h.sendln(h.serverMessages, err.Error())
		h.Logger().Error(h.user, err)
		commandFinished()
		return
	}

	maprMessages, closeMaprMessages := h.newGeneratedMaprMessagesChannel(sessionGenerationFromContext(ctx))
	// Bind the destination before publishing the aggregate pointer. Graceful
	// shutdown waits for admitted command initialization and can therefore
	// never observe an aggregate whose Start goroutine has not published its
	// result channel yet.
	aggregate.PrepareOutput(ctx, maprMessages)
	// Use the atomic setter so concurrent reads from Shutdown, Aggregate,
	// and resetSessionAggregates are race-free.
	h.setAggregate(aggregate)
	go func() {
		defer recoverHandlerPanic(h.Logger(), h.user, "map command cleanup", h.abortAfterPanic)
		defer commandFinished()
		defer closeMaprMessages()
		defer recoverHandlerPanic(h.Logger(), h.user, "map command", h.abortAfterPanic)
		command.Start(ctx, maprMessages)
	}()
}

func (h *ServerHandler) handleAckUserCommand(_ context.Context, _ lcontext.LContext, argc int, args []string, commandFinished func()) {
	h.handleAckCommand(argc, args)
	commandFinished()
}

func (h *ServerHandler) handleAuthKeyCommand(_ context.Context, _ lcontext.LContext,
	argc int, args []string, commandFinished func()) {

	defer commandFinished()

	if !h.serverCfg.AuthKeyEnabled {
		h.sendln(h.serverMessages, "AUTHKEY ERR feature disabled")
		return
	}

	if config.IsPasswordOnlyUser(h.user.Name) {
		h.sendln(h.serverMessages, "AUTHKEY ERR unsupported user")
		return
	}

	if argc < 2 || strings.TrimSpace(args[1]) == "" {
		h.sendln(h.serverMessages, "AUTHKEY ERR missing public key")
		return
	}

	decodedPubKey, err := base64.StdEncoding.DecodeString(args[1])
	if err != nil {
		h.sendln(h.serverMessages, "AUTHKEY ERR invalid base64")
		return
	}

	pubKey, err := gossh.ParsePublicKey(decodedPubKey)
	if err != nil {
		h.sendln(h.serverMessages, "AUTHKEY ERR invalid public key")
		return
	}

	if h.authKeyStore == nil {
		h.sendln(h.serverMessages, "AUTHKEY ERR internal key store unavailable")
		return
	}
	h.authKeyStore.Add(h.user.Name, pubKey)
	h.sendln(h.serverMessages, "AUTHKEY OK")
}

func (h *ServerHandler) newGeneratedMaprMessagesChannel(generation uint64) (chan string, func()) {
	maprMessages := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverHandlerPanic(h.Logger(), h.user, "map output forwarding", h.abortAfterPanic)
		for {
			select {
			case message, ok := <-maprMessages:
				if !ok {
					return
				}
				h.send(h.maprMessages, encodeGeneratedMessage(generation, message))
			case <-h.outputAbortDone():
				return
			case <-h.done.Done():
				return
			}
		}
	}()
	return maprMessages, func() {
		close(maprMessages)
		<-done
	}
}

// GracefulShutdownContext gracefully drains serverless output while ctx says
// the client output consumer is available. If the consumer fails, teardown
// switches to Shutdown so blocked producers and forwarding goroutines are
// released without depending on an unread protocol queue.
func (h *ServerHandler) GracefulShutdownContext(ctx context.Context) {
	if ctx == nil {
		panic("handlers: nil graceful shutdown context")
	}

	h.stopCommandAdmission()
	if ctx.Err() != nil {
		h.Shutdown()
		return
	}
	initializationDone := make(chan struct{})
	go func() {
		defer close(initializationDone)
		defer recoverHandlerPanic(h.Logger(), h.user, "command initialization wait", h.abortAfterPanic)
		h.commandInitWg.Wait()
	}()
	select {
	case <-initializationDone:
	case <-ctx.Done():
		h.Shutdown()
		return
	}
	if ctx.Err() != nil {
		h.Shutdown()
		return
	}

	ta := h.getAggregate()
	if ta != nil {
		ta.PrepareShutdown(ctx)
	}
	h.cancelCommandWork()
	if ta != nil {
		h.Logger().Info(h.user, "Finalizing serverless output aggregate")
		ta.Shutdown(ctx)
	}
	if ctx.Err() != nil {
		h.Shutdown()
		return
	}

	commandsDone := make(chan struct{})
	go func() {
		defer close(commandsDone)
		defer recoverHandlerPanic(h.Logger(), h.user, "command completion wait", h.abortAfterPanic)
		h.commandWg.Wait()
	}()
	select {
	case <-commandsDone:
	case <-ctx.Done():
		h.Shutdown()
		return
	}
	if err := h.flushOutput(ctx); err != nil {
		h.reportFlushError(0, fmt.Errorf("flush direct output: %w", err))
	}
	if ctx.Err() != nil {
		h.Shutdown()
		return
	}
	h.done.Shutdown()
}
