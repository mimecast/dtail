package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
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
	readHub             *readhub.Hub
	serverCfg           *config.ServerConfig
	readTimings         readTimings
	authKeyStore        *authkey.Store
	colorizer           Colorizer
	regex               string
	commands            map[string]commandHandler
	sessionState        sessionCommandState
	commandBatch        commandBatch
	idleShutdownStarted atomic.Bool
	// escapedFramingAdvertised is set when the session's capabilities frame
	// lists protocol.CapabilityEscapedFramingV1, the only case in which the
	// client's protocol.EscapedFramingCommand switches the session framing.
	escapedFramingAdvertised bool
	// mapQueryFailures records which command streams had a failed map
	// command, so that their reads are rejected (see mapqueryfailure.go).
	mapQueryFailures mapQueryFailures
	// mapParserFactory creates the log format parser of a map command; nil
	// means logformat.NewParserWithHostname. Tests replace it to make
	// building a map command fail after its query parsed.
	mapParserFactory mapParserFactory
	// controlFramesAdvertised is set when the capabilities frame lists
	// protocol.CapabilityControlFramesV1, the only case in which the
	// client's protocol.ControlFramesCommand marks the session's control
	// frames.
	controlFramesAdvertised bool
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
	// dserver checks this once at startup already (server.New); the check
	// stays here for every other runtime that builds a handler.
	if err := ValidateOutputBufferConfig(serverCfg); err != nil {
		return nil, fmt.Errorf("create server handler: %w", err)
	}

	h := ServerHandler{
		baseHandler: newBaseHandler(ctx, baseHandlerConfig{
			logger:       loggers.Diagnostics,
			readerLogger: loggers.Reader,
			// Only the in-process client runtime provides an output writer,
			// and it alone selects serverless mode, so dserver sessions
			// always stay on the transport path.
			serverlessOutput:    dependencies.ServerlessOutput,
			user:                user,
			maxCommandFrameSize: serverCfg.MaxCommandFrameSize,
		}),
		catLimiter:   dependencies.CatLimiter,
		tailLimiter:  dependencies.TailLimiter,
		readHub:      dependencies.ReadHub,
		serverCfg:    serverCfg,
		readTimings:  newReadTimings(serverCfg),
		authKeyStore: dependencies.AuthKeyStore,
		colorizer:    dependencies.Colorizer,
		regex:        ".",
		escapedFramingAdvertised: slices.Contains(dependencies.Capabilities,
			protocol.CapabilityEscapedFramingV1),
		controlFramesAdvertised: slices.Contains(dependencies.Capabilities,
			protocol.CapabilityControlFramesV1),
	}
	h.handleCommandCb = h.handleUserCommand
	h.prepareCommandContextCb = h.prepareCommandContext
	h.commandRejectedCb = h.commandRejected
	h.frameRejectedCb = h.frameRejected
	h.versionRejectedCb = h.versionRejected
	h.sessionClosingCb = h.idleShutdownStarted.Load
	h.commands = h.newCommandRegistry()
	h.output.configure(h.outputManagerConfig(), loggers.Diagnostics)
	h.activeGeneration = h.sessionState.currentGeneration

	s := strings.Split(dependencies.Hostname, ".")
	h.hostname = s[0]
	h.send(h.serverMessages, protocol.HiddenCapabilitiesPrefix+strings.Join(dependencies.Capabilities, " "))

	return &h, nil
}

// ValidateOutputBufferConfig checks that OutputBufferMaxBytes leaves room for
// a line of MaxLineLength bytes after protocol framing, so that a valid
// maximum-length line is not rejected. Zero values use the safe defaults. A
// MaxLineLength so large that the required minimum does not fit an int is
// rejected as well: the minimum would otherwise wrap around to a negative
// number and let every OutputBufferMaxBytes pass. dserver calls it once at
// startup, so that such a configuration fails there instead of refusing every
// session, and NewServerHandler calls it for every handler.
func ValidateOutputBufferConfig(serverCfg *config.ServerConfig) error {
	maxLineLength := positiveIntOrDefault(serverCfg.MaxLineLength, 1024*1024)
	bufferMaxBytes := positiveIntOrDefault(serverCfg.OutputBufferMaxBytes, defaultOutputBufferMaxBytes)
	minimum, ok := minimumOutputBufferMaxBytes(maxLineLength)
	if !ok {
		return fmt.Errorf("MaxLineLength %d is too large: "+
			"OutputBufferMaxBytes must be at least MaxLineLength + %d, which exceeds the largest supported value %d",
			maxLineLength, outputBufferLineOverhead, math.MaxInt)
	}
	if bufferMaxBytes < minimum {
		return fmt.Errorf("OutputBufferMaxBytes %d must be at least %d for MaxLineLength %d",
			bufferMaxBytes, minimum, maxLineLength)
	}
	return nil
}

// outputBufferLineOverhead is what OutputBufferMaxBytes must hold beyond one
// maximum-length line: a flush can contain the tail of the normal 64 KiB
// writer batch plus a maximum-length line, so another full batch is reserved
// as bounded framing/path overhead.
const outputBufferLineOverhead = 2 * networkWriterBufferSize

// minimumOutputBufferMaxBytes is the smallest OutputBufferMaxBytes accepted
// for maxLineLength, maxLineLength + outputBufferLineOverhead. The output
// queue copies rather than adopts a batch whose spare capacity would crowd
// out the next one (see tryEnqueueLocked), so two full batches still queue at
// this minimum. It reports false when the sum does not fit an int.
func minimumOutputBufferMaxBytes(maxLineLength int) (int, bool) {
	if maxLineLength > math.MaxInt-outputBufferLineOverhead {
		return 0, false
	}
	return maxLineLength + outputBufferLineOverhead, true
}

// escapedOutputBufferMaxBytes is the output buffer cap of a session that
// uses escaped framing: minimumOutputBufferMaxBytes plus another
// maxLineLength, i.e. 2*maxLineLength + outputBufferLineOverhead. Escaping
// replaces each 0xAC and 0xFF byte by two bytes, so the frame of a
// maximum-length line can grow to almost twice its raw size, and the batch
// holding it with it; this cap admits that batch just as the minimum admits
// the raw one. A session switching to escaped framing raises
// its cap to this value when OutputBufferMaxBytes is smaller, so every
// configuration valid for raw framing keeps working, at a higher memory
// bound of about one more maximum-length line per session. It reports false
// when the sum does not fit an int; the session then keeps the configured
// cap (which is then at least about half of math.MaxInt anyway).
//
// It builds on minimumOutputBufferMaxBytes, so the escaped cap always exceeds
// the raw minimum ValidateOutputBufferConfig enforces by exactly one
// maxLineLength, and both report an overflow the same way.
func escapedOutputBufferMaxBytes(maxLineLength int) (int, bool) {
	minimum, ok := minimumOutputBufferMaxBytes(maxLineLength)
	if !ok || maxLineLength > math.MaxInt-minimum {
		return 0, false
	}
	return minimum + maxLineLength, true
}

func (h *ServerHandler) handleUserCommand(ctx context.Context, ltx lcontext.LContext,
	argc int, args []string, commandName string) {

	h.Logger().Debug(h.user, "Handling user command", argc, argsForLog(args))
	if strings.EqualFold(commandName, ".ack") {
		h.handleAckControl(ctx, argc, args)
		return
	}
	if !h.beginCommand(hasSessionCommandAdmission(ctx)) {
		cancelCommandContext(ctx)
		return
	}
	markCommandAdmitted(ctx)
	if commandName == "SESSION" {
		ctx = withSessionCommandAdmission(ctx)
	}
	commandFinished := h.newCommandFinished(ctx, shouldShutdownOnCommandCompletion(commandName))
	defer recoverHandlerPanic(h.Logger(), h.user, "command dispatch cleanup", h.abortAfterPanic)
	defer h.finishCommandInitialization()
	defer recoverHandlerPanic(h.Logger(), h.user, "command dispatch", func() {
		defer h.abortAfterPanic()
		commandFinished()
	})

	handler, found := h.commands[commandName]
	if !found {
		h.sendln(h.serverMessages, logging.ErrorMessage(h.Logger(), h.user,
			"Received unknown user command", commandName, argc, argsForLog(args)))
		h.reportCommandFailure(commandFailureUnknown)
		commandFinished()
		return
	}

	handler(ctx, ltx, argc, args, commandFinished)
}

// newCommandFinished returns the commandFinished callback of an admitted
// command: it finishes the command once (finishUserCommand), however often
// and from whichever goroutine it is called.
func (h *ServerHandler) newCommandFinished(ctx context.Context, shutdownOnCompletion bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() { h.finishUserCommand(ctx, shutdownOnCompletion) })
	}
}

// finishUserCommand ends an admitted command. When it was the session's last
// work it starts the idle shutdown, which drains the output and then runs the
// close handshake. AUTHKEY and SESSION are session-side commands and do not
// end the session (shouldShutdownOnCommandCompletion), as user commands may
// still follow in it.
//
// Whether the session is idle is decided, and the idle shutdown claimed,
// right here (claimIdleShutdown), while the counts read are still current:
// a command inside an open input batch must not end the session, also not
// once the batch completed while a later command of it runs. Only the
// shutdown itself runs on a goroutine of its own (idleShutdownAfterCommand).
// A command can finish on the command input goroutine, e.g. an unknown
// command or a failed map query, and that goroutine also handles the
// client's close acknowledgement: a shutdown on it would block until the
// acknowledgement timeout (5 s) passed. A command queues its output before
// it calls commandFinished, and the shutdown flushes everything queued
// before it requests the close, so the close handshake still comes after all
// of the session's output.
//
// The command's context and its commandWg slot are only released once the
// shutdown returned, as when it ran inline: the shutdown uses that context,
// and graceful shutdown waits on commandWg.
func (h *ServerHandler) finishUserCommand(ctx context.Context, shutdownOnCompletion bool) {
	handedOver := false
	defer func() {
		if !handedOver {
			h.finishCommand()
		}
	}()
	activeCommands := h.decrementActiveCommands()
	pendingFiles := atomic.LoadInt32(&h.pendingFiles)
	h.Logger().Debug(h.user, "Command finished", "activeCommands", activeCommands, "pendingFiles", pendingFiles)

	if shutdownOnCompletion && h.claimIdleShutdown(pendingFiles, activeCommands) {
		handedOver = true
		go h.idleShutdownAfterCommand(ctx)
		return
	}
	// Release this command's child context. The handler's connection-derived
	// root handles session-wide cancellation directly, without a watcher
	// goroutine per command.
	cancelCommandContext(ctx)
}

// idleShutdownAfterCommand runs the idle shutdown finishUserCommand claimed,
// then releases the finished command's context and commandWg slot.
func (h *ServerHandler) idleShutdownAfterCommand(ctx context.Context) {
	defer h.finishCommand()
	defer cancelCommandContext(ctx)
	defer recoverHandlerPanic(h.Logger(), h.user, "idle shutdown", h.abortAfterPanic)
	// A command that ran into its own deadline ("timeout N ...") leaves an
	// expired context behind. The shutdown's flushes and its wait for the
	// close acknowledgement would all return at once with it, and the
	// session closed while its last output and the close frame were still
	// on their way. The connection's context bounds the shutdown instead.
	shutdownCtx := ctx
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && h.connCtx != nil {
		shutdownCtx = h.connCtx
	}
	h.shutdown(shutdownCtx)
}

// handleAckControl handles the ".ack" control commands. The close
// acknowledgement completes an existing shutdown handshake; it is protocol
// control traffic rather than new workload. All of them are processed even
// after graceful shutdown has sealed command admission, and kept out of
// commandWg so shutdown never waits on its own acknowledgement. Unknown ones
// are ignored (handleAckCommand), which is what lets a client send
// protocol.EscapedFramingCommand and protocol.ControlFramesCommand to
// servers that do not know them.
func (h *ServerHandler) handleAckControl(ctx context.Context, argc int, args []string) {
	defer cancelCommandContext(ctx)
	switch {
	case isInputBatchBegin(argc, args):
		h.BeginCommandBatch()
	case isInputBatchComplete(argc, args):
		h.completeCommandBatch(h.connCtx)
	case isEscapedFramingRequest(argc, args):
		h.handleEscapedFramingRequest(ctx)
	case isControlFramesRequest(argc, args):
		h.handleControlFramesRequest()
	default:
		h.handleAckCommand(argc, args)
	}
}

// handleEscapedFramingRequest switches the session to escaped framing when
// its capabilities frame advertised it and the request is the session's
// first command, as clients send it. Otherwise the request is ignored, as by
// a server that does not know it, and the session keeps its framing: a
// switch after other commands could change the framing of output those
// commands already produce, under a client that decided its framing from
// the session's first frame. A switch also raises the session's output
// buffer cap to escapedOutputBufferMaxBytes, so that escaping can never make
// a batch with a maximum-length line too large to queue.
func (h *ServerHandler) handleEscapedFramingRequest(ctx context.Context) {
	switch {
	case !h.escapedFramingAdvertised:
		h.Logger().Debug(h.user, "Ignoring escaped framing request of a session not advertising it")
		return
	case !h.handlingCommandFrame(1):
		h.Logger().Warn(h.user, "Ignoring escaped framing request that is not the session's first command")
		return
	}
	if !h.enableEscapedFraming(ctx) {
		return
	}
	h.Logger().Debug(h.user, "Switched session to escaped framing")
	if !h.controlFramesAdvertised {
		// No control frames request can switch the session any more.
		h.settleFraming()
	}
	maxLineLength := positiveIntOrDefault(h.serverCfg.MaxLineLength, 1024*1024)
	if minimum, ok := escapedOutputBufferMaxBytes(maxLineLength); ok {
		h.output.raiseBufferMaxBytes(minimum)
	}
}

// handleControlFramesRequest switches the session to
// protocol.EscapedControlFraming when its capabilities frame advertised it,
// the session is escaped and the request is its second command, as clients
// send it right after the escaped framing request. The session then marks
// every control frame it sends, so the client never takes a --plain log line
// for one. Otherwise the request is ignored, as by a server that does not
// know it, and the session keeps its framing: the client marks control
// frames only when the session's first frame advertised both capabilities,
// and output of earlier commands than the escaped framing request could
// already be framed unmarked. Either way the session framing is final
// afterwards, so the frames Read held back since the capabilities frame go
// out (settleFraming).
func (h *ServerHandler) handleControlFramesRequest() {
	defer h.settleFraming()
	switch {
	case !h.controlFramesAdvertised:
		h.Logger().Debug(h.user, "Ignoring control frames request of a session not advertising it")
	case !h.handlingCommandFrame(2):
		h.Logger().Warn(h.user, "Ignoring control frames request that is not the session's second command")
	case !h.enableControlFrames():
		h.Logger().Warn(h.user, "Ignoring control frames request of a session without escaped framing")
	default:
		h.Logger().Debug(h.user, "Switched session to escaped framing with control frames")
	}
}

// isFramingRequest reports whether a decoded command is one of the framing
// requests, the only commands that can switch the session framing.
func isFramingRequest(argc int, args []string) bool {
	return isEscapedFramingRequest(argc, args) || isControlFramesRequest(argc, args)
}

func isEscapedFramingRequest(argc int, args []string) bool {
	return argc == 3 && strings.Join(args, " ") == protocol.EscapedFramingCommand
}

func isControlFramesRequest(argc int, args []string) bool {
	return argc == 3 && strings.Join(args, " ") == protocol.ControlFramesCommand
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
	// The marker is processed by the sole client-to-server writer, which
	// must stay free to deliver the close acknowledgement.
	h.startIdleShutdown(ctx)
}

// frameRejected ends a session whose command frame the dispatcher rejected
// before any handler ran (commandDispatcher.frameRejected: undecodable frame,
// invalid timeout prefix or unparsable options), when that left the session
// without work. No command was admitted for such a frame, so
// finishUserCommand never runs for it, and without this a session whose only
// command was rejected stayed open until IdleSessionTimeoutS. The frame's
// error and failure report are already queued, so the close handshake still
// follows them. A frame rejected inside an open input batch, or while other
// commands run or a SESSION keeps the session alive, closes nothing
// (claimIdleShutdown): the batch end or the last command to finish does.
func (h *ServerHandler) frameRejected() {
	h.startIdleShutdown(h.connCtx)
}

// versionRejected ends a session after a command frame of another protocol
// version was rejected (commandDispatcher.rejectProtocolVersion), when that
// left the session without work, like frameRejected, but without the close
// handshake (closeWithoutHandshake): the client may not recognize or
// acknowledge it. It does not run for a legacy client (protocol <= 3, an
// unparsable version or no protocol prefix), whose session stays open. Its
// mismatch error is already queued and is read before the session ends. The
// claim also makes the dispatcher drop later frames of that version without
// another error (sessionClosingCb).
func (h *ServerHandler) versionRejected() {
	h.startIdleEnd(h.connCtx, h.closeWithoutHandshake)
}

// startIdleShutdown starts the session's idle shutdown with the close
// handshake (shutdown), like startIdleEnd.
func (h *ServerHandler) startIdleShutdown(ctx context.Context) {
	h.startIdleEnd(ctx, h.shutdown)
}

// startIdleEnd claims the session's idle shutdown if the session is idle
// (claimIdleShutdown) and runs end, which closes the session, on a goroutine
// of its own. Callers run on the command input goroutine, the sole
// client-to-server writer, which also handles the client's close
// acknowledgement: a shutdown on it would wait for the acknowledgement
// timeout (5 s). The claim is made here, synchronously, while the counts it
// depends on are still current.
//
// end holds a commandWg slot (beginShutdownWork) while it runs, like the
// shutdown finishUserCommand hands over, so Shutdown and
// GracefulShutdownContext wait for it. That cannot deadlock: Shutdown closes
// h.done before it waits, and every wait of h.shutdown and
// h.closeWithoutHandshake (flush, close request, close acknowledgement) also
// ends on h.done; GracefulShutdownContext falls back to Shutdown when its
// context ends. If command admission was sealed after the claim, the
// teardown that sealed it ends the session and end does not run.
func (h *ServerHandler) startIdleEnd(ctx context.Context, end func(context.Context)) {
	if !h.claimIdleShutdown(h.PendingAndActive()) {
		return
	}
	if !h.beginShutdownWork() {
		h.Logger().Debug(h.user, "Not starting the idle shutdown of a stopping session")
		return
	}
	go func() {
		defer h.finishCommand()
		defer recoverHandlerPanic(h.Logger(), h.user, "idle shutdown", h.abortAfterPanic)
		end(ctx)
	}()
}

// claimIdleShutdown reports whether the session is idle, given its pending
// file and active command counts, and if so claims its one idle shutdown;
// the caller must then end the session (h.shutdown, or
// h.closeWithoutHandshake for a version mismatch, see startIdleEnd). The
// session is not idle while it is kept alive, stopping, or inside an open
// input batch, whose remaining commands may still follow. Callers that end
// the session on another goroutine claim it here first, so a later change
// of these conditions cannot be missed by a check the goroutine would only
// make later.
func (h *ServerHandler) claimIdleShutdown(pendingFiles, activeCommands int32) bool {
	if pendingFiles != 0 || activeCommands != 0 || h.sessionState.keepAlive() ||
		h.isStopping() || h.commandBatch.isOpen() {
		return false
	}
	return h.idleShutdownStarted.CompareAndSwap(false, true)
}

// shouldShutdownOnCommandCompletion reports whether the session may end once
// commandName completed. AUTHKEY and SESSION keep it open. The names are
// matched exactly, like the command table: matched case-insensitively,
// "Session" or "AuthKey" were reported as unknown commands and then still
// kept the session open until the idle timeout.
func shouldShutdownOnCommandCompletion(commandName string) bool {
	switch commandName {
	case "AUTHKEY", "SESSION":
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
		// Decided at dispatch: commands are dispatched in the order they
		// arrive, so a map command sent before this read already failed.
		command.mapQueryFailed = h.mapQueryFailures.failed(sessionGenerationFromContext(ctx))
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
	command, aggregate, err := h.mapCommandFor(ctx, args)
	if err != nil {
		h.sendln(h.serverMessages, h.messageText(err.Error()))
		h.Logger().Error(h.user, err)
		h.reportCommandFailure(commandFailureMapQuery)
		h.mapQueryFailures.mark(sessionGenerationFromContext(ctx))
		commandFinished()
		return
	}
	h.mapQueryFailures.clear(sessionGenerationFromContext(ctx))

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

// commandRejected learns of a command rejected before its handler ran, e.g.
// for options that do not parse. A rejected map command failed like one whose
// query is invalid, so the reads of its command stream are rejected as well
// (see mapQueryFailures) instead of streaming plain payload.
func (h *ServerHandler) commandRejected(ctx context.Context, commandName string) {
	if commandName == "map" {
		h.mapQueryFailures.mark(sessionGenerationFromContext(ctx))
	}
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
