package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// maprMessageQueueSize is the default capacity of the session's queue of
// MapReduce messages (one per serialized group). Read frames all queued mapr
// messages up to its buffer size into one SSH channel write
// (sessionFramer.readMaprMessage), so the queue bounds how many groups one
// write can carry: a 200k-group result took about 149k write(2) calls
// without coalescing, 10-19k with 10 slots, 6.6k with 64 and 2.2-2.7k with
// 256 (about 10% less dserver CPU time than with 10 slots).
//
// The queue is bounded by count, not by bytes, and lies outside
// OutputBufferMaxBytes, which only bounds read payload. A queued message is
// one serialized group: its group key and one value per select field. These
// are typically a few dozen bytes, but a key or a value such as last() of a
// field is taken from a log line and can be up to MaxLineLength bytes (1 MiB
// by default) each. In the worst case a session holds (256 + 16 + 1)
// messages of that size outside OutputBufferMaxBytes: this queue, the
// 16-entry forwarding channel of the map command
// (newGeneratedMaprMessagesChannel) and the message its forwarder holds,
// i.e. up to 273 * (1 + select fields) * MaxLineLength bytes, plus up to
// one Read buffer and one frame in sessionFramer.readBuf. That is a share
// of what the aggregate itself already holds, unbounded, for every group
// while it serializes them. A byte budget is left out deliberately: such
// groups need a query grouping by or selecting values of near-maximal lines.
const maprMessageQueueSize = 256

type baseHandlerConfig struct {
	logger       logging.Logger
	readerLogger logging.Logger
	// serverlessOutput is the in-process runtime's own output. It is the
	// single source of serverless mode: a non-nil writer selects it, nil
	// keeps the session on its transport. There is deliberately no separate
	// serverless flag, so no caller can ask for serverless mode without
	// saying where the payload goes.
	serverlessOutput    io.Writer
	user                *user.User
	serverMessages      chan string
	maprMessages        chan string
	maxCommandFrameSize int
	hostname            string
	activeGeneration    func() uint64
}

type baseHandler struct {
	logger           logging.Logger
	readerLogger     logging.Logger
	serverlessOutput io.Writer
	// connCtx is the SSH or in-process connection lifetime. commandRootCtx is
	// its session-work child, canceled for handler-wide command replacement or
	// shutdown without canceling the owning transport.
	connCtx        context.Context
	commandRootCtx context.Context
	cancelCommands context.CancelFunc
	done           *internal.Done
	user           *user.User

	// aggregate is written by handleMapCommand on the command-dispatch
	// goroutine and read concurrently by Shutdown, Aggregate, and
	// resetSessionAggregates. Using atomic.Pointer eliminates the data race.
	aggregate atomic.Pointer[mapaggregate.Aggregate]

	*sessionFramer
	*commandDispatcher
	*outputCoordinator
}

// newBaseHandler builds the three focused session components and binds them to
// one stable handler pointer. Keeping this wiring in one place prevents partial
// handlers whose components disagree about the session they serve.
func newBaseHandler(connCtx context.Context, cfg baseHandlerConfig) *baseHandler {
	if connCtx == nil {
		panic("handlers: nil connection context")
	}
	commandRootCtx, cancelCommands := context.WithCancel(connCtx)
	serverMessages := cfg.serverMessages
	if serverMessages == nil {
		serverMessages = make(chan string, 10)
	}
	maprMessages := cfg.maprMessages
	if maprMessages == nil {
		maprMessages = make(chan string, maprMessageQueueSize)
	}

	h := &baseHandler{
		logger:           logging.OrNop(cfg.logger),
		readerLogger:     logging.OrNop(cfg.readerLogger),
		serverlessOutput: cfg.serverlessOutput,
		connCtx:          connCtx,
		commandRootCtx:   commandRootCtx,
		cancelCommands:   cancelCommands,
		done:             internal.NewDone(),
		user:             cfg.user,
	}
	h.sessionFramer = newSessionFramer(h, cfg.maxCommandFrameSize)
	h.commandDispatcher = &commandDispatcher{
		handler: h,
		codec:   newProtocolCodec(cfg.user, cfg.logger),
		// Serverless mode is a property of the runtime that built this
		// handler, never of the session it serves: it holds exactly when
		// that runtime handed over an output of its own.
		serverless: cfg.serverlessOutput != nil,
	}
	h.outputCoordinator = &outputCoordinator{
		handler:          h,
		maprMessages:     maprMessages,
		serverMessages:   serverMessages,
		hostname:         cfg.hostname,
		ackCloseReceived: make(chan struct{}),
		outputAbort:      internal.NewDone(),
		activeGeneration: cfg.activeGeneration,
	}
	// Set before any goroutine uses the manager, like its configuration.
	h.outputCoordinator.output.clientSlow = h.outputCoordinator.clientSlow
	return h
}

// Logger returns the handler logger, falling back to a no-op logger for
// focused unit tests that do not need diagnostics.
func (h *baseHandler) Logger() logging.Logger {
	return logging.OrNop(h.logger)
}

// ReaderLogger returns the logger used by filesystem reader dependencies.
func (h *baseHandler) ReaderLogger() logging.Logger {
	return logging.OrNop(h.readerLogger)
}

// Shutdown aborts all session work and waits for admitted commands to stop.
func (h *baseHandler) Shutdown() {
	h.commandMu.Lock()
	h.stopping = true
	h.aborting = true
	h.outputAbort.Shutdown()
	h.cancelCommands()
	if aggregate := h.getAggregate(); aggregate != nil {
		aggregate.Abort()
	}
	h.commandMu.Unlock()

	if aggregate := h.getAggregate(); aggregate != nil {
		h.Logger().Info(h.user, "Aborting output aggregate")
		aggregate.AbortAndWait(h.connCtx)
	}
	h.done.Shutdown()
	h.commandWg.Wait()
}

// Done returns the handler shutdown channel.
func (h *baseHandler) Done() <-chan struct{} {
	return h.done.Done()
}

// getAggregate returns the current output MapReduce aggregate atomically.
func (h *baseHandler) getAggregate() *mapaggregate.Aggregate {
	return h.aggregate.Load()
}

// setAggregate stores an output MapReduce aggregate atomically.
func (h *baseHandler) setAggregate(aggregate *mapaggregate.Aggregate) {
	h.aggregate.Store(aggregate)
}

// shutdown gracefully drains output, performs the close acknowledgement
// handshake, and then closes the session.
func (h *baseHandler) shutdown(ctx context.Context) {
	if ctx == nil {
		panic("handlers: nil shutdown context")
	}
	activeCommands := atomic.LoadInt32(&h.activeCommands)
	h.Logger().Info(h.user, "shutdown() called", "activeCommands", activeCommands,
		"outputMode", h.output.enabled())

	h.drainSessionOutput(ctx)
	h.requestCloseSync(ctx)
	h.waitForCloseAcknowledgement(ctx)
	h.cancelCommands()
	h.done.Shutdown()
}

// closeWithoutHandshake drains output like shutdown and then closes the
// session without the close handshake (no ".syn close connection", no wait
// for its acknowledgement). It ends sessions whose client speaks another
// protocol version, which can not be relied on to recognize or acknowledge
// the handshake; a legacy client (protocol <= 3, an unparsable version or no
// protocol prefix) keeps its session instead
// (commandDispatcher.rejectProtocolVersion). The client sees
// the queued error and then the end of the session without its close
// handshake, i.e. an incomplete session. Read still hands out everything
// queued before it reports EOF.
func (h *baseHandler) closeWithoutHandshake(ctx context.Context) {
	if ctx == nil {
		panic("handlers: nil close context")
	}
	h.Logger().Info(h.user, "Closing session of another protocol version without close handshake")
	h.drainSessionOutput(ctx)
	h.cancelCommands()
	h.done.Shutdown()
}

// drainSessionOutput flushes the session's direct output, shuts its output
// aggregate down and waits until the client read everything queued, or the
// flush timeout passed; flush errors are reported to the client.
func (h *baseHandler) drainSessionOutput(ctx context.Context) {
	if h.output.enabled() {
		if err := h.flushOutput(ctx); err != nil {
			h.reportFlushError(0, fmt.Errorf("flush direct output: %w", err))
		}
	}

	if aggregate := h.getAggregate(); aggregate != nil {
		h.Logger().Info(h.user, "Shutting down output aggregate in shutdown()")
		aggregate.Shutdown(ctx)
		h.reportIncompleteMapResult(aggregate)
	}

	if err := h.flushContext(ctx); err != nil {
		h.reportFlushError(0, err)
	}
}

// Reasons a session command reports with reportCommandFailure; the read
// command has its own (see readCommand.reportFailure).
const (
	commandFailureProtocol      = protocol.CommandFailureDecode
	commandFailureRejected      = protocol.CommandFailureRejected
	commandFailureUnknown       = protocol.CommandFailureUnknown
	commandFailureMapQuery      = protocol.CommandFailureMapQuery
	commandFailureMapIncomplete = protocol.CommandFailureMapIncomplete
)

// reportIncompleteMapResult reports the session's map command as failed when
// the final serialization of its aggregate ran but was interrupted, so that
// some groups never reached the output. shutdown calls it before the close
// handshake, so the client never takes a result with missing groups for a
// complete one: a scheduled job writes no outfile, as for every failure that
// is no file read failure, and interactive dmap warns.
//
// It is a defensive guard. The final serialization has no deadline of its
// own, only the finalization context ends it early, and the context ends
// that do (session teardown, dserver shutdown, an idle connection) tear the
// session down rather than close it gracefully, so the client sees a session
// cut short anyway. An aggregate that never finalized is not reported: one
// aborted by a canceled command or a replaced session generation has no
// final result, and neither has one whose finalization still runs when
// Shutdown returned because ctx ended (the session is ending then).
// The aggregate already logged the interruption at WARN, with the number of
// unsent groups; this logs the report at INFO only.
func (h *baseHandler) reportIncompleteMapResult(aggregate *mapaggregate.Aggregate) {
	if !aggregate.FinalResultInterrupted() {
		return
	}
	h.Logger().Info(h.user, "MapReduce final result did not reach the client in full, reporting a failed command")
	h.reportCommandFailure(commandFailureMapIncomplete)
}

// reportCommandFailure tells the client that a command of the session failed
// (see protocol.HiddenCommandFailedPrefix). Like the error message sent before
// it, it is queued before the command ends and so reaches the client before
// the session's close handshake.
func (h *baseHandler) reportCommandFailure(reason string) {
	h.sendln(h.serverMessages, protocol.HiddenCommandFailedPrefix+reason)
}

// messageText returns text, which may contain text the client sent such as
// its query, for a server message whose content is not fixed, e.g. a session
// error acknowledgement or a map command's diagnostic. Under EscapedFraming
// and EscapedControlFraming every byte of a frame body is transported as it
// is (control frames are escaped too, and their marker is unforgeable), so
// text is returned unchanged. Raw framing (clients that did not negotiate
// escaped framing, such as older ones) has no escaping: a character whose
// UTF-8 encoding contains the message delimiter byte would split the message,
// so such characters, and control characters, are replaced with blanks.
func (h *baseHandler) messageText(text string) string {
	if h.framing() != protocol.RawFraming {
		return text
	}
	return rawFramingText(text)
}

// rawFramingText replaces the characters of text that raw framing can not
// carry in a message, and control characters, with blanks.
func rawFramingText(text string) string {
	return strings.Map(func(r rune) rune {
		var encoded [utf8.UTFMax]byte
		n := utf8.EncodeRune(encoded[:], r)
		if unicode.IsControl(r) || bytes.IndexByte(encoded[:n], protocol.MessageDelimiter) >= 0 {
			return ' '
		}
		return r
	}, text)
}

// abortAfterPanic signals every session-owned producer and consumer to stop.
// It deliberately does not wait because recovery can run inside commandWg.
func (h *baseHandler) abortAfterPanic() {
	h.commandMu.Lock()
	h.stopping = true
	h.aborting = true
	h.outputAbort.Shutdown()
	h.cancelCommands()
	aggregate := h.getAggregate()
	h.commandMu.Unlock()
	if aggregate != nil {
		aggregate.Abort()
	}
	h.done.Shutdown()
}

var _ Handler = (*baseHandler)(nil)
