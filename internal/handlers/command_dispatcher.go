package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
)

const maxCommandTimeoutSeconds = 24 * 60 * 60

type handleCommandCb func(context.Context, lcontext.LContext, int, []string, string)
type prepareCommandContextCb func(context.Context, string) (context.Context, func())
type commandRejectedCb func(context.Context, string)
type frameRejectedCb func()
type versionRejectedCb func(mismatchedClient)
type sessionClosingCb func() bool

type commandCancelKeyType struct{}
type sessionCommandAdmissionKeyType struct{}
type commandAdmissionResultKeyType struct{}

type commandAdmissionResult struct {
	admitted bool
}

type commandDispatcher struct {
	handler                 *baseHandler
	handleCommandCb         handleCommandCb
	prepareCommandContextCb prepareCommandContextCb
	// commandRejectedCb, if set, learns of a command that dispatchCommand
	// rejected before its handler ran, by command name.
	commandRejectedCb commandRejectedCb
	// frameRejectedCb, if set, runs after handleCommand rejected a command
	// frame of the session's protocol version before any handler ran
	// (undecodable frame, or a dispatchCommand error such as an invalid
	// timeout prefix or unparsable options) and queued its error and
	// failure report. No command was admitted for such a frame, so no
	// command finishes and nothing else would notice that the session may
	// have no work left.
	frameRejectedCb frameRejectedCb
	// versionRejectedCb, if set, runs after handleCommand rejected a command
	// frame of another protocol version and queued its error
	// (rejectProtocolVersion), with the kind of client that sent it. The
	// handler decides whether the session stays open (older clients, see
	// mismatchedClient) or ends without the close handshake. Without it the
	// session stays open.
	versionRejectedCb versionRejectedCb
	// sessionClosingCb, if set, reports whether the session's end (with or
	// without the close handshake) was already started or claimed. A frame
	// of another protocol version arriving then gets no error message, so
	// that nothing of the session follows its ".syn close connection" or
	// its end.
	sessionClosingCb sessionClosingCb
	// rejectionsDroppedCb, if set, reports whether a command frame of the
	// session's protocol version that handleCommand rejects (undecodable
	// frame or dispatchCommand error) is dropped without its error, failure
	// report and frameRejectedCb. Only the health handler sets it, for the
	// frames after its one command (HealthHandler.shutdownStarted), so that
	// nothing but the close handshake follows that command's answer.
	rejectionsDroppedCb sessionClosingCb
	// versionErrorSent records that the session got its one protocol
	// version mismatch error (rejectProtocolVersion); later frames of
	// another version get none.
	versionErrorSent atomic.Bool

	codec          protocolCodec
	activeCommands int32
	commandMu      sync.Mutex
	commandInitWg  sync.WaitGroup
	commandWg      sync.WaitGroup
	stopping       bool
	aborting       bool

	// optionsOnce applies the quiet/plain options of the session's first
	// command carrying options (handleOptions); later ones change nothing.
	optionsOnce sync.Once
	// quiet and plain are set by handleOptions on the command input
	// goroutine, and read concurrently by the output goroutine
	// (sessionFramer.readServerMessage frames the server messages queued
	// before the first command carrying options) and by the read command
	// goroutines building their line writers
	// (ServerHandler.newReadLineWriter), some of which started before that
	// command. Hence atomics; read them with quietMode and plainMode. A
	// server message framed before the options were handled still goes out
	// as a tagged server message, as it always could. Control messages such
	// as an AUTHKEY reply are marked control frames whatever the plain mode
	// (sessionFramer.readServerMessage), so the options never change them.
	quiet atomic.Bool
	plain atomic.Bool
	// serverless is set once when the handler is built (newBaseHandler) and
	// never changes, so it needs no synchronization.
	serverless bool
}

var commandCancelKey commandCancelKeyType
var sessionCommandAdmissionKey sessionCommandAdmissionKeyType
var commandAdmissionResultKey commandAdmissionResultKeyType

func withSessionCommandAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionCommandAdmissionKey, true)
}

func hasSessionCommandAdmission(ctx context.Context) bool {
	admitted, _ := ctx.Value(sessionCommandAdmissionKey).(bool)
	return admitted
}

func markCommandAdmitted(ctx context.Context) {
	if result, ok := ctx.Value(commandAdmissionResultKey).(*commandAdmissionResult); ok {
		result.admitted = true
	}
}

func withCommandCancel(ctx context.Context, cancel context.CancelFunc) context.Context {
	if cancel == nil {
		return ctx
	}
	return context.WithValue(ctx, commandCancelKey, cancel)
}

func cancelCommandContext(ctx context.Context) {
	cancel, ok := ctx.Value(commandCancelKey).(context.CancelFunc)
	if !ok || cancel == nil {
		return
	}
	cancel()
}

func applyCommandTimeout(ctx context.Context, args []string, argc int) (context.Context, []string, int, error) {
	if argc < 3 || args[0] != "timeout" {
		return ctx, args, argc, nil
	}

	seconds, err := strconv.Atoi(args[1])
	if err != nil {
		return ctx, args, argc, fmt.Errorf("invalid timeout value %q: %w", args[1], err)
	}
	if seconds > maxCommandTimeoutSeconds {
		return ctx, args, argc, fmt.Errorf("timeout value %d exceeds maximum of %d seconds",
			seconds, maxCommandTimeoutSeconds)
	}
	if seconds <= 0 {
		return ctx, args[2:], argc - 2, nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	parentCancel, _ := ctx.Value(commandCancelKey).(context.CancelFunc)
	combined := func() {
		cancel()
		if parentCancel != nil {
			parentCancel()
		}
	}

	return withCommandCancel(timeoutCtx, combined), args[2:], argc - 2, nil
}

func (d *commandDispatcher) handleCommand(command string) {
	h := d.handler
	h.Logger().Debug(h.user, commandForLog(command))

	args, argc, add, client, err := d.codec.handleProtocolVersion(strings.Split(command, " "))
	if err != nil {
		d.rejectProtocolVersion(add, client, err)
		return
	}
	args, argc, err = d.codec.handleBase64(args, argc)
	if err != nil {
		if d.dropRejection(err) {
			return
		}
		h.sendln(h.serverMessages, logging.ErrorMessage(h.Logger(), h.user, err))
		h.reportCommandFailure(commandFailureProtocol)
		d.frameRejected()
		return
	}

	ctx, cancel := d.newCommandContext()
	ctx = withCommandCancel(ctx, cancel)
	if dispatchErr := d.dispatchCommand(ctx, args, argc); dispatchErr != nil {
		cancel()
		if d.dropRejection(dispatchErr) {
			return
		}
		h.sendln(h.serverMessages, logging.ErrorMessage(h.Logger(), h.user, dispatchErr))
		h.reportCommandFailure(commandFailureRejected)
		d.frameRejected()
	}
}

// rejectProtocolVersion handles a command frame of another protocol version
// (or of no version, i.e. without a protocol prefix). It queues the mismatch
// error followed by add, the separator the client expects after it, once per
// session: later frames of another version get none (versionErrorSent), and
// none at all once the session is closing (sessionClosingCb), so nothing of
// the session follows its end or its close handshake. There is no
// failed-command message here: a client of another protocol version may not
// know hidden messages or their framing.
//
// How the session goes on is up to the handler (versionRejectedCb), which
// learns the kind of client (protocolCodec.handleProtocolVersion):
//   - Older clients keep their session (ServerHandler.versionRejected:
//     protocol major version <= 4, an unparsable version, or no protocol
//     prefix at all, as v1.x clients send; HealthHandler.versionRejected:
//     the same without 4.x): the session ends when the client exits or at
//     IdleSessionTimeoutS. Some of them reconnect after a
//     session ended (see mismatchedClient), so ending it would make them
//     reconnect in a loop, each time with a full SSH handshake and server
//     log lines.
//   - Any other client of another version (e.g. a future one) gets the
//     session ended without the close handshake (closeWithoutHandshake): it
//     can not be relied on to read the handshake's ".syn close connection"
//     as such or to acknowledge it, as its framing may differ from this
//     server's, so the session would wait for the close acknowledgement
//     timeout (5 s). Ending the session after the error instead shows it
//     the error and an incomplete session at once.
func (d *commandDispatcher) rejectProtocolVersion(add string, client mismatchedClient, err error) {
	h := d.handler
	if d.sessionClosingCb != nil && d.sessionClosingCb() {
		h.Logger().Debug(h.user, "Dropping frame of another protocol version after the close", err)
		return
	}
	if d.versionErrorSent.CompareAndSwap(false, true) {
		h.send(h.serverMessages, logging.ErrorMessage(h.Logger(), h.user, err)+add)
	} else {
		h.Logger().Debug(h.user, "Dropping further frame of another protocol version", err)
	}
	if d.versionRejectedCb != nil {
		d.versionRejectedCb(client)
	}
}

// dropRejection reports whether a rejected command frame is dropped
// silently (rejectionsDroppedCb) and logs it at debug level if so.
func (d *commandDispatcher) dropRejection(err error) bool {
	if d.rejectionsDroppedCb == nil || !d.rejectionsDroppedCb() {
		return false
	}
	h := d.handler
	h.Logger().Debug(h.user, "Dropping rejected command frame after the session's end", err)
	return true
}

// frameRejected tells frameRejectedCb, if set, that handleCommand rejected a
// command frame. It runs after the frame's error and failure report were
// queued, so a shutdown it starts still sends them before the close
// handshake.
func (d *commandDispatcher) frameRejected() {
	if d.frameRejectedCb != nil {
		d.frameRejectedCb()
	}
}

func (d *commandDispatcher) dispatchCommand(ctx context.Context, args []string, argc int) error {
	ctx, args, argc, err := applyCommandTimeout(ctx, args, argc)
	if err != nil {
		return err
	}

	parts := strings.SplitN(args[0], ":", 2)
	commandName := parts[0]
	ltx := lcontext.LContext{}
	if len(parts) == 2 && len(parts[1]) != 0 {
		options, parsedContext, deserializeErr := config.DeserializeOptions([]string{parts[1]})
		if deserializeErr != nil {
			if d.commandRejectedCb != nil {
				d.commandRejectedCb(ctx, commandName)
			}
			return deserializeErr
		}
		d.handleOptions(options)
		ltx = parsedContext
		ctx = d.withReadShareOption(ctx, options[config.ReadShareOption])
	}

	if d.prepareCommandContextCb != nil {
		var cleanup func()
		ctx, cleanup = d.prepareCommandContextCb(ctx, commandName)
		if cleanup != nil {
			defer cleanup()
		}
	}

	d.handleCommandCb(ctx, ltx, argc, args, commandName)
	return nil
}

// handleRawCommand dispatches one command of a SESSION START/UPDATE spec,
// which session.Spec.Commands built, without protocol or base64 framing.
//
// It splits on single spaces exactly like the base64-decoded command path in
// handleCommand, which gets the same commands from the same builder. A read
// command's regex is the rejoined args[2:], so splitting on runs of
// whitespace (strings.Fields) would drop trailing blanks, collapse blank runs
// and turn tabs into spaces in the pattern. The builder emits single spaces
// between fields, so it needs no tolerance for extra ones. An empty pattern
// is serialized with a trailing space, "regex:noop " (every line) or, with
// --invert, "regex:invert " (no line), and its trailing empty field
// rejoins to the same regex the command path gets; dropping that field would
// leave readCommand.parseRegex to read the lone flags word as an empty
// pattern. A read command's path is always the single word args[1]: the
// builder encodes a path containing a space (protocol.EncodeReadPath) and
// readCommand.parsePath decodes it.
func (d *commandDispatcher) handleRawCommand(ctx context.Context, command string) error {
	if command == "" {
		return fmt.Errorf("empty command")
	}
	args := strings.Split(command, " ")
	return d.dispatchCommand(ctx, args, len(args))
}

func (d *commandDispatcher) newCommandContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(d.handler.commandRootCtx)
}

func (d *commandDispatcher) handleAckCommand(argc int, args []string) {
	h := d.handler
	if argc < 3 {
		if !d.quietMode() {
			h.sendln(h.serverMessages, logging.WarnMessage(h.Logger(), h.user,
				"Unable to parse command", args, argc))
		}
		return
	}
	if args[1] == "close" && args[2] == "connection" {
		h.acknowledgeClose()
	}
}

// quietMode reports whether the session asked for quiet mode. It is safe
// for concurrent use.
func (d *commandDispatcher) quietMode() bool {
	return d.quiet.Load()
}

// plainMode reports whether the session asked for plain mode. It is safe
// for concurrent use, e.g. by the output goroutine framing server messages.
func (d *commandDispatcher) plainMode() bool {
	return d.plain.Load()
}

// handleOptions applies the session options of the first command carrying
// options. sync.Once serializes concurrent callers, and the atomics publish
// the modes to the goroutines reading them (see the quiet and plain fields).
func (d *commandDispatcher) handleOptions(options map[string]string) {
	h := d.handler
	d.optionsOnce.Do(func() {
		if quiet := options["quiet"]; quiet == "true" {
			h.Logger().Debug(h.user, "Enabling quiet mode")
			d.quiet.Store(true)
		}
		if plain := options["plain"]; plain == "true" {
			h.Logger().Debug(h.user, "Enabling plain mode")
			d.plain.Store(true)
		}
		// The "serverless" option is deliberately not honoured here. It
		// decides whether the session payload is written to this process'
		// own stdout instead of the session transport, which only the
		// in-process serverless runtime may do. It is derived once from the
		// runtime (Dependencies.ServerlessOutput) when the handler is built,
		// so a remote client cannot divert its payload into the dserver log.
		if options["serverless"] == "true" && !d.serverless {
			h.Logger().Debug(h.user, "Ignoring serverless option of a non-serverless session")
		}
	})
}

func (d *commandDispatcher) beginCommand(admittedSessionWork bool) bool {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	if d.aborting || (d.stopping && !admittedSessionWork) {
		return false
	}
	d.commandWg.Add(1)
	d.commandInitWg.Add(1)
	atomic.AddInt32(&d.activeCommands, 1)
	return true
}

// beginShutdownWork admits a session shutdown that runs on a goroutine of
// its own (ServerHandler.startIdleEnd) into commandWg, like a command,
// so that Shutdown and GracefulShutdownContext wait for it as they wait for
// the shutdown a finished command hands over (finishUserCommand). Like
// beginCommand it adds under commandMu, and only while command admission is
// open: Shutdown and GracefulShutdownContext seal admission under commandMu
// before they wait, so no Add can race their Wait. It reports false once
// admission is sealed; the teardown that sealed it then ends the session.
// The caller must call finishCommand once the shutdown returned.
func (d *commandDispatcher) beginShutdownWork() bool {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	if d.aborting || d.stopping {
		return false
	}
	d.commandWg.Add(1)
	return true
}

func (d *commandDispatcher) finishCommandInitialization() {
	d.commandInitWg.Done()
}

func (d *commandDispatcher) decrementActiveCommands() int32 {
	atomic.AddInt32(&d.activeCommands, -1)
	return atomic.LoadInt32(&d.activeCommands)
}

func (d *commandDispatcher) finishCommand() {
	d.commandWg.Done()
}

func (d *commandDispatcher) isStopping() bool {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	return d.stopping
}

func (d *commandDispatcher) stopCommandAdmission() {
	d.commandMu.Lock()
	d.stopping = true
	d.commandMu.Unlock()
}

func (d *commandDispatcher) cancelCommandWork() {
	d.commandMu.Lock()
	d.handler.cancelCommands()
	d.commandMu.Unlock()
}
