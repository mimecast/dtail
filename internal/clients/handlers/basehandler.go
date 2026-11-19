package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
)

// maxRetainedLineBufBytes is the largest lineBuf capacity kept for reuse after
// a message. Ordinary log lines stay far below it and reuse the buffer without
// allocating; a rare huge message without a trailing newline (for example a
// piece of a line longer than MaxLineLength, split by the server) must not pin
// its backing array for the lifetime of every server connection.
const maxRetainedLineBufBytes = 64 * 1024

// frameKind tells how the client handles a received frame (see
// handleMessage).
type frameKind uint8

const (
	// framePayload is an unmarked frame: it is never a control message,
	// whatever its content.
	framePayload frameKind = iota
	// frameControl is a marked control frame.
	frameControl
	// frameIgnored is the unmarked hidden first frame of a server of an
	// older protocol version (see checkFirstFrame): reported, not printed.
	frameIgnored
)

// sessionFraming is the framing of every frame a protocol 5 server sends,
// from the session's first frame on; there is no negotiation.
const sessionFraming = protocol.EscapedControlFraming

type baseHandler struct {
	done         *internal.Done
	server       string
	shellStarted bool
	commands     chan string
	receiveBuf   bytes.Buffer

	// pendingCommand holds the unsent tail of a dequeued command frame. A
	// frame (e.g. a large base64-encoded regex or MapReduce query) can exceed
	// the buffer io.Copy hands to Read (32KB), so the remainder is kept here
	// and drained by subsequent Read calls instead of being dropped, which
	// would corrupt the server-side command stream. Only touched by Read
	// (single output-copy goroutine).
	pendingCommand []byte
	// lineBuf is scratch space for appending the newline to a payload message
	// that lacks one. It is released after a message that grew it beyond
	// maxRetainedLineBufBytes. Only touched by Write (single input-copy
	// goroutine).
	lineBuf []byte
	// frameBuf is scratch space for decoding an escaped frame (see
	// decodeFrame); released like lineBuf. Only touched by Write.
	frameBuf []byte
	// status is the handler's exit status so far (see Status): -1 (none
	// yet) for client and mapreduce sessions, 2 (CRITICAL) for health
	// checks until the server answers OK. It is atomic, as it is written by
	// the goroutine copying the server's output into Write (checkFirstFrame,
	// the health check's OK) and by the dial goroutine (ReportServerError,
	// e.g. a missing journal-v1 capability), and read by the client.
	status atomic.Int32

	// expectCapabilities is set for the sessions whose first frame must be
	// the server's capabilities frame (client and mapreduce sessions, not
	// health checks; see checkFirstFrame). firstFrameSeen is set once the
	// first frame was received, so only that frame is checked. It is only
	// touched by Write.
	expectCapabilities bool
	firstFrameSeen     bool

	capabilitiesMu sync.RWMutex
	capabilities   map[string]struct{}
	capabilitiesCh chan struct{}
	capabilitiesOk sync.Once

	sessionAcks chan SessionAck
	// onSessionAcked, if set, is called when the server acknowledged a
	// SESSION START or UPDATE (no rejection), on the goroutine handling the
	// received frames and before the acknowledgement is queued for
	// WaitForSessionAck: the frames after it belong to the acknowledged
	// workload (see MaprHandler).
	onSessionAcked func()
	// busy measures the time Write spends handling received frames, during
	// which no further frame, e.g. a session acknowledgement, is read (see
	// WaitForSessionAck).
	busy busyClock
	// probeAnswers queues the probe answers the server sent (see
	// handleProbeMessage and Probe); nil for handlers that never probe
	// (health checks). probeSlot (capacity 1) lets one Probe call send
	// and wait at a time; the others wait for it (see Probe).
	probeAnswers chan protocol.ProbeAnswer
	probeSlot    chan struct{}
	logger       clientlog.Logger

	// closeReceived is set when the server started the session's close
	// handshake, i.e. the session ended normally (see Outcome).
	closeReceived atomic.Bool
	failureMu     sync.Mutex
	// failures are the distinct reasons of the failed commands the server
	// reported, in the order of their first report.
	failures []string
}

// maxReportedFailures bounds how many distinct failure reasons a session
// keeps; servers send a few fixed ones.
const maxReportedFailures = 16

// SessionOutcome is how the server ended a session, as far as the client
// knows.
type SessionOutcome struct {
	// Completed is true when the server ended the session with its close
	// handshake, after it had sent all of the session's output. A session
	// that ended otherwise (connection lost, server killed or shut down, the
	// client canceled it) is incomplete. An incomplete session the client
	// did not cancel itself makes dcat and dgrep exit with status 1 (see
	// clients.exitStatusCutShort).
	Completed bool
	// Failures are the distinct reasons of the commands the server reported
	// as failed (protocol.HiddenCommandFailedPrefix), in the order of their
	// first report. Any failure makes
	// dcat and dgrep exit with status 2 (see clients.ExitStatusFailedCommand).
	Failures []string
}

// SessionAck is a parsed hidden acknowledgement for SESSION START/UPDATE requests.
type SessionAck struct {
	Action     string
	Generation uint64
	Error      string
}

func (h *baseHandler) String() string {
	return fmt.Sprintf("baseHandler(%s,server:%s,shellStarted:%v,status:%d)@%p",
		h.done,
		h.server,
		h.shellStarted,
		h.status.Load(),
		h,
	)
}

func (h *baseHandler) Server() string {
	return h.server
}

// Status returns the handler's exit status so far (see baseHandler.status);
// safe for concurrent use.
func (h *baseHandler) Status() int {
	return int(h.status.Load())
}

func (h *baseHandler) log() clientlog.Logger {
	if h.logger == nil {
		return clientlog.NopLogger{}
	}
	return h.logger
}

func (h *baseHandler) Capabilities() []string {
	h.capabilitiesMu.RLock()
	defer h.capabilitiesMu.RUnlock()

	capabilities := make([]string, 0, len(h.capabilities))
	for capability := range h.capabilities {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	return capabilities
}

func (h *baseHandler) HasCapability(name string) bool {
	h.capabilitiesMu.RLock()
	defer h.capabilitiesMu.RUnlock()

	_, ok := h.capabilities[name]
	return ok
}

// Outcome returns how the session ended so far; call it once the handler is
// done.
func (h *baseHandler) Outcome() SessionOutcome {
	h.failureMu.Lock()
	defer h.failureMu.Unlock()
	return SessionOutcome{Completed: h.closeReceived.Load(), Failures: slices.Clone(h.failures)}
}

// ReportServerError reports a server error to the user and raises a status
// of 0 or less (none yet, or OK) to 1; a higher status (a health check's
// CRITICAL) is kept. It is safe for concurrent use: the stdout-copy goroutine
// (checkFirstFrame) and the dial goroutine (requireJournalCapability) may
// both report an error of the same session.
func (h *baseHandler) ReportServerError(message string) {
	h.raiseStatusToFailed()
	// Route through the DIAGNOSTIC (Log) sink, not Raw: a server-error report is
	// an audit line, not bulk payload. Via Raw it would be gated out of the
	// client log file whenever Client.LogPayload is false (the default), silently
	// dropping the error from the on-disk audit trail. RawLog keeps it in the
	// file like other diagnostics while still printing it to stdout. The message
	// carries no trailing newline; the Log sink appends one.
	clientlog.RawDiagnostic(h.log(), formatServerErrorMessage(h.server, message))
}

// raiseStatusToFailed sets the status to 1 unless it is above 0 already, in
// one atomic step, so a concurrent higher status is never overwritten.
func (h *baseHandler) raiseStatusToFailed() {
	for {
		current := h.status.Load()
		if current > 0 || h.status.CompareAndSwap(current, 1) {
			return
		}
	}
}

// SendMessage to the server. It waits up to 5 seconds for the session to
// take the command and returns nil without sending it once the handler is
// done.
func (h *baseHandler) SendMessage(command string) error {
	return h.sendMessage(context.Background(), command)
}

// sendMessage is SendMessage, which also gives up when ctx ends (with ctx's
// error).
func (h *baseHandler) sendMessage(ctx context.Context, command string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	// A scheduled job's read share stays out of the log, in the command and
	// in its encoding.
	loggedCommand, loggedEncoded := command, encoded
	if redacted := config.RedactReadShare(command); redacted != command {
		loggedCommand, loggedEncoded = redacted, "(redacted)"
	}
	h.logSending(command, loggedCommand, loggedEncoded)

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case h.commands <- commandFrame(encoded):
	case <-timer.C:
		return fmt.Errorf("timed out sending command '%s' (base64: '%s')", loggedCommand, loggedEncoded)
	case <-h.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// sessionCommandPrefix starts the SESSION START and SESSION UPDATE commands
// (session.Spec).
const sessionCommandPrefix = "SESSION "

// logSending logs a command about to be sent, at the debug level. A SESSION
// command's line is not waited for: the interactive control loop sends its
// SESSION UPDATEs here, and while a host key question pauses the client
// logger (only at the debug log level does this line block) it is the only
// goroutine that can answer the question. Every other command is logged in
// order, as before.
func (h *baseHandler) logSending(command, loggedCommand, loggedEncoded string) {
	if strings.HasPrefix(command, sessionCommandPrefix) {
		go h.log().Debug("Sending command", h.server, loggedCommand, loggedEncoded)
		return
	}
	h.log().Debug("Sending command", h.server, loggedCommand, loggedEncoded)
}

// commandFrame frames a base64-encoded command for the server.
func commandFrame(encodedCommand string) string {
	return fmt.Sprintf("protocol %s base64 %v;", protocol.ProtocolCompat, encodedCommand)
}

// decodeFrame returns the body of a received frame (without its delimiter)
// in the session framing (sessionFraming), and how to handle it (see
// frameKind). The result aliases frame or frameBuf and is only valid until
// the next decodeFrame call; frame itself is never modified. The control
// frame marker is cut off before the frame is decoded, as a decoded body may
// start with the same bytes. The session's first frame is checked
// (checkFirstFrame).
func (h *baseHandler) decodeFrame(frame []byte) ([]byte, frameKind) {
	frame, control := sessionFraming.CutControl(frame)
	kind := framePayload
	if control {
		kind = frameControl
	}
	decoded := sessionFraming.DecodeFrame(frame, &h.frameBuf)
	if cap(h.frameBuf) > maxRetainedLineBufBytes {
		// decoded stays valid: only the handler's reference is dropped, so
		// an oversized frame does not pin its buffer (same policy as lineBuf).
		h.frameBuf = nil
	}
	if !h.firstFrameSeen {
		h.firstFrameSeen = true
		kind = h.checkFirstFrame(decoded, kind)
	}
	return decoded, kind
}

// checkFirstFrame checks the session's first frame, of the given kind, and
// returns how to handle it. A protocol 5 server starts every client and
// mapreduce session with its capabilities frame, a marked control frame
// starting with protocol.HiddenCapabilitiesPrefix (which may list no
// capability at all). Any other first frame comes from a server of an older
// protocol version, which frames nothing the way this client decodes it and
// may finish the session before reading the workload: the session is
// reported as failed (status 1), so that it never looks like a successful
// empty query. Such a server's unmarked hidden message (e.g. its unmarked
// capabilities frame) is ignored instead of printed; any other first frame,
// e.g. the server's version mismatch error, is handled as usual. Health check
// sessions have no capabilities frame and are not checked.
func (h *baseHandler) checkFirstFrame(first []byte, kind frameKind) frameKind {
	if !h.expectCapabilities {
		return kind
	}
	if kind == frameControl && bytes.HasPrefix(first, []byte(protocol.HiddenCapabilitiesPrefix)) {
		return kind
	}
	h.ReportServerError("unsupported DTail server: not a protocol " + protocol.ProtocolCompat +
		" server (no capabilities frame first); please update DTail server")
	if kind == framePayload && protocol.IsHiddenMessage(string(first)) {
		return frameIgnored
	}
	return kind
}

// Write receives data from the dtail server via the io.Writer interface.
//
// The stream is a sequence of messages terminated by protocol.MessageDelimiter;
// newlines are ordinary payload bytes. Each message is decoded in the session
// framing (decodeFrame) before it is handled; as every frame is escaped, no
// payload byte equals the delimiter, so a line containing e.g. '€' (whose
// UTF-8 encoding ends in the delimiter byte 0xAC) stays one message. The
// frames are cut out of the stream by scanFrames. handleMessage and
// everything below it must not retain the slice (io.Writer forbids retaining
// p).
func (h *baseHandler) Write(p []byte) (n int, err error) {
	// Measured as busy time for WaitForSessionAck (busyClock), a payload
	// write blocked by the paused output included.
	h.busy.begin()
	defer h.busy.end()
	h.scanFrames(p, h.handleFrame)
	return len(p), nil
}

// handleFrame decodes and handles one received frame.
func (h *baseHandler) handleFrame(frame []byte) {
	h.handleMessage(h.decodeFrame(frame))
}

// scanFrames calls handle with every frame that p completes, without its
// protocol.MessageDelimiter and not yet decoded (decodeFrame), in stream
// order. p is scanned with bytes.IndexByte instead of byte by byte; as every frame
// is escaped, the delimiter byte only ever ends a frame, so an escape
// sequence (FrameEscape and its code) may be split across chunks like any
// other bytes. A frame that lies completely inside p is passed as a sub-slice
// of p without copying; only the unfinished tail of a chunk is kept in
// receiveBuf until a later chunk completes it. The frame is only valid for
// the duration of the handle call. Only Write calls it, on the single
// stdout-copy goroutine.
func (h *baseHandler) scanFrames(p []byte, handle func(frame []byte)) {
	for len(p) > 0 {
		end := bytes.IndexByte(p, protocol.MessageDelimiter)
		if end < 0 {
			h.receiveBuf.Write(p)
			return
		}
		if h.receiveBuf.Len() == 0 {
			handle(p[:end])
		} else {
			h.receiveBuf.Write(p[:end])
			handle(h.receiveBuf.Bytes())
			// Like lineBuf and frameBuf, do not keep the backing array of
			// an unusually large frame for the life of the connection.
			if h.receiveBuf.Cap() > maxRetainedLineBufBytes {
				h.receiveBuf = bytes.Buffer{}
			} else {
				h.receiveBuf.Reset()
			}
		}
		p = p[end+1:]
	}
}

// Send data to the dtail server via Reader interface.
//
// Priority select: when Done() is closed we must still drain any pending
// commands before returning io.EOF, because closing the connection requires
// the '.ack close connection' message to be flushed to the server first.
// Without this drain the server waits up to 5 seconds for the ack.
//
// Command frames larger than p are delivered across multiple Read calls via
// pendingCommand (see consumeCommand); the leftover is always drained before
// a new command is dequeued so frames are never truncated or interleaved.
func (h *baseHandler) Read(p []byte) (n int, err error) {
	if len(h.pendingCommand) > 0 {
		n = copy(p, h.pendingCommand)
		h.pendingCommand = h.pendingCommand[n:]
		if len(h.pendingCommand) == 0 {
			// Release the backing array once the frame is fully delivered.
			// Keeping it would retain the largest-ever frame for the handler
			// lifetime and let the slice creep forward through its backing
			// array on reuse; oversized frames are rare, so consumeCommand
			// simply allocates a fresh buffer next time instead.
			h.pendingCommand = nil
		}
		return
	}

	// Check for a pending command first (non-blocking), giving it priority
	// over the Done signal so that queued acks are always delivered.
	select {
	case command := <-h.commands:
		return h.consumeCommand(p, command), nil
	default:
	}

	// No command is immediately ready; block on whichever arrives first.
	select {
	case command := <-h.commands:
		return h.consumeCommand(p, command), nil
	case <-h.Done():
		return 0, io.EOF
	}
}

// consumeCommand copies as much of command as fits into p and stashes the
// remainder in pendingCommand so the next Read calls can deliver the rest of
// the frame. pendingCommand is always empty here (Read drains and releases it
// before dequeuing a new command), so a fresh buffer is allocated for the
// remainder rather than reusing a long-lived one.
func (h *baseHandler) consumeCommand(p []byte, command string) int {
	n := copy(p, command)
	if n < len(command) {
		h.pendingCommand = []byte(command[n:])
	}
	return n
}

// handleMessage routes one decoded message of the given kind. A marked
// control frame (hidden messages and AUTHKEY acknowledgements) is consumed
// here; every unmarked frame is user payload, whatever its content (so a
// --plain log line such as ".syn close connection" is printed, not acted
// upon), and is written with exactly one trailing newline (writePayload). An
// ignored first frame (frameIgnored) is dropped. message is only valid for
// the duration of the call.
func (h *baseHandler) handleMessage(message []byte, kind frameKind) {
	switch kind {
	case frameControl:
		h.handleControlMessage(message)
	case frameIgnored:
		h.log().Debug(h.server, "Ignoring first frame of an unsupported server", string(message))
	default:
		h.writePayload(message)
	}
}

// handleControlMessage handles the body of a marked control frame. A body
// that is neither a hidden message nor an AUTHKEY reply comes from a newer
// server and is ignored, like an unknown hidden message.
func (h *baseHandler) handleControlMessage(message []byte) {
	if len(message) > 0 && message[0] == '.' {
		h.handleHiddenMessage(string(message))
		return
	}
	if !h.handleAuthKeyMessage(string(message)) {
		h.log().Debug(h.server, "Ignoring unknown control frame", string(message))
	}
}

// writePayload writes one payload message with exactly one trailing newline.
func (h *baseHandler) writePayload(message []byte) {
	if len(message) > 0 && message[len(message)-1] == '\n' {
		clientlog.RawBytes(h.log(), message)
		return
	}
	// The newline must reach the sink in the same call as the message, or lines
	// from concurrent server connections could interleave between the two. p
	// cannot be extended in place (io.Writer must not modify it), so the line is
	// assembled in a scratch buffer reused across messages.
	h.lineBuf = append(append(h.lineBuf[:0], message...), '\n')
	clientlog.RawBytes(h.log(), h.lineBuf)
	if cap(h.lineBuf) > maxRetainedLineBufBytes {
		// Same policy as pendingCommand in Read: drop an oversized backing
		// array and let the next message allocate a normal-sized one.
		h.lineBuf = nil
	}
}

func (h *baseHandler) handleAuthKeyMessage(message string) bool {
	isAuthKeyMessage, authKeyOK, authKeyDetail := parseAuthKeyMessage(message)
	if !isAuthKeyMessage {
		return false
	}

	if authKeyOK {
		h.log().Debug(h.server, "AUTHKEY registration accepted by server")
		return true
	}

	if authKeyDetail == "" {
		h.log().Warn(h.server, "AUTHKEY registration failed")
		return true
	}

	h.log().Warn(h.server, "AUTHKEY registration failed", authKeyDetail)
	return true
}

// parseAuthKeyMessage parses the body of a marked control frame as an AUTHKEY
// reply ("AUTHKEY OK" or "AUTHKEY ERR <detail>"), surrounding blanks
// ignored. The server sends AUTHKEY replies only as bare control frames, never
// wrapped in a SERVER frame.
func parseAuthKeyMessage(message string) (isAuthKeyMessage bool, ok bool, detail string) {
	payload := strings.TrimSpace(message)
	switch {
	case payload == "AUTHKEY OK":
		return true, true, ""
	case strings.HasPrefix(payload, "AUTHKEY ERR"):
		detail := strings.TrimSpace(strings.TrimPrefix(payload, "AUTHKEY ERR"))
		return true, false, detail
	default:
		return false, false, ""
	}
}

// Handle messages received from server which are not meant to be displayed
// to the end user.
func (h *baseHandler) handleHiddenMessage(message string) {
	switch {
	case strings.HasPrefix(message, protocol.HiddenCapabilitiesPrefix):
		h.handleCapabilitiesMessage(message)
	case strings.HasPrefix(message, protocol.HiddenSessionStartOKPrefix),
		strings.HasPrefix(message, protocol.HiddenSessionUpdateOKPrefix),
		strings.HasPrefix(message, protocol.HiddenSessionErrorPrefix):
		h.handleSessionAckMessage(message)
	case strings.HasPrefix(message, protocol.HiddenCommandFailedPrefix):
		h.handleCommandFailedMessage(message)
	case strings.HasPrefix(message, protocol.HiddenProbePrefix):
		h.handleProbeMessage(message)
	case strings.HasPrefix(message, ".syn close connection"):
		h.closeReceived.Store(true)
		if err := h.SendMessage(".ack close connection"); err != nil {
			h.log().Debug(h.server, "Unable to acknowledge close connection", err)
		}
		h.Shutdown()
	}
}

func (h *baseHandler) handleCommandFailedMessage(message string) {
	reason := strings.TrimSpace(strings.TrimPrefix(message, protocol.HiddenCommandFailedPrefix))
	if reason == "" {
		reason = "unknown reason"
	}
	h.log().Debug(h.server, "Server reported a failed command", reason)
	h.failureMu.Lock()
	defer h.failureMu.Unlock()
	if len(h.failures) < maxReportedFailures && !slices.Contains(h.failures, reason) {
		h.failures = append(h.failures, reason)
	}
}

func (h *baseHandler) handleCapabilitiesMessage(message string) {
	capabilities := strings.Fields(strings.TrimPrefix(message, protocol.HiddenCapabilitiesPrefix))

	h.capabilitiesMu.Lock()
	defer h.capabilitiesMu.Unlock()

	if h.capabilities == nil {
		h.capabilities = make(map[string]struct{})
	}
	for _, capability := range capabilities {
		if capability == "" {
			continue
		}
		h.capabilities[capability] = struct{}{}
	}

	h.capabilitiesOk.Do(func() {
		if h.capabilitiesCh != nil {
			close(h.capabilitiesCh)
		}
	})
}

func (h *baseHandler) Done() <-chan struct{} {
	return h.done.Done()
}

// WaitForCapabilities reports whether the server's capabilities arrived,
// waiting up to timeout for them. Capabilities that already arrived win
// without any wait: the non-blocking check comes first because a select with
// several ready cases picks one at random, so a closed capabilities channel
// could otherwise lose against an expired timer (a descheduled goroutine under
// load) or an already finished session.
func (h *baseHandler) WaitForCapabilities(timeout time.Duration) bool {
	if h.capabilitiesCh == nil {
		return false
	}

	select {
	case <-h.capabilitiesCh:
		return true
	default:
	}
	if timeout <= 0 {
		return false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-h.capabilitiesCh:
		return true
	case <-h.Done():
		return false
	case <-timer.C:
		return false
	}
}

// WaitForSessionAck returns the next session acknowledgement, waiting for
// one until the handler was idle for timeout, but never longer than
// maxWait (wall clock; 0 for no cap). Like WaitForCapabilities, an
// acknowledgement that is already queued wins without any wait: the
// non-blocking receive comes first because a select with several ready
// cases picks one at random, so a queued acknowledgement could otherwise
// lose against an expired timer or an already finished session.
//
// Only the time the handler was free to read frames counts against timeout:
// the time Write was busy (busyClock) does not, so a busy handler stretches
// the wait. dserver sends the acknowledgement ahead of the payload it has
// queued and of all output of the workload it acknowledges
// (handlers.sessionAckGate), but payload frames already on their way can
// come before it, and while the interactive output is paused, the handler's
// payload write blocks and the acknowledgement is not read. So pausing the
// output right after a SESSION START, for longer than the timeout, does not
// fail the session start: its wait (maxWait 0) goes on until the output
// resumes. A server that never answers times out after timeout of idle
// handler time; the session's end ends the wait, and so does the end of the
// connection's context for the start (WaitForSessionAckContext). A SESSION
// UPDATE runs on the interactive control loop,
// which must not wait without bound (e.g. for a host key prompt that only
// the control loop answers while it pauses the output), so it passes a
// maxWait (connectors.sessionUpdateAckMaxWait).
func (h *baseHandler) WaitForSessionAck(timeout, maxWait time.Duration) (SessionAck, bool) {
	return h.WaitForSessionAckContext(context.Background(), timeout, maxWait)
}

// WaitForSessionAckContext is WaitForSessionAck whose wait also ends once
// ctx is done (a queued acknowledgement still wins). The initial SESSION
// START passes its connection's context: its wait has no wall-clock cap, so
// without it a :quit or Ctrl-C while the start was unacknowledged returned
// only after timeout of idle handler time, or, with the output paused, not
// before the output ran again.
func (h *baseHandler) WaitForSessionAckContext(ctx context.Context,
	timeout, maxWait time.Duration) (SessionAck, bool) {
	if h.sessionAcks == nil {
		return SessionAck{}, false
	}

	select {
	case ack := <-h.sessionAcks:
		return ack, true
	default:
	}
	if timeout <= 0 {
		return SessionAck{}, false
	}
	return h.waitForSessionAckIdle(ctx, timeout, maxWait)
}

// minAckRecheck is the shortest time the acknowledgement wait's timer is
// armed again for, so that a little idle time still missing does not make
// it spin.
const minAckRecheck = 10 * time.Millisecond

// waitForSessionAckIdle waits for a session acknowledgement until the
// handler was idle (not busy in Write) for timeout since the call, maxWait
// (if above 0) passed, the session ended or ctx is done. When the timer
// fires early because of busy time, it is armed again for the idle time
// still missing (at least minAckRecheck), but not past maxWait.
func (h *baseHandler) waitForSessionAckIdle(ctx context.Context,
	timeout, maxWait time.Duration) (SessionAck, bool) {
	start := time.Now()
	busyAtStart := h.busy.busy(start)
	timer := time.NewTimer(min(timeout, remainingWait(maxWait, 0)))
	defer timer.Stop()

	for {
		select {
		case ack := <-h.sessionAcks:
			return ack, true
		case <-h.Done():
			// An acknowledgement already queued still wins: select picks
			// one of several ready cases at random, and a server may
			// acknowledge and end its session in one write.
			return h.WaitForSessionAck(0, 0)
		case <-ctx.Done():
			// As above: a queued acknowledgement still wins.
			return h.WaitForSessionAck(0, 0)
		case <-timer.C:
			now := time.Now()
			elapsed := now.Sub(start)
			idle := elapsed - (h.busy.busy(now) - busyAtStart)
			if idle >= timeout || (maxWait > 0 && elapsed >= maxWait) {
				// An acknowledgement queued meanwhile still wins.
				return h.WaitForSessionAck(0, 0)
			}
			timer.Reset(min(max(timeout-idle, minAckRecheck), remainingWait(maxWait, elapsed)))
		}
	}
}

// remainingWait returns how much of maxWait is left after elapsed, or the
// largest duration for no cap (maxWait 0).
func remainingWait(maxWait, elapsed time.Duration) time.Duration {
	if maxWait <= 0 {
		return time.Duration(math.MaxInt64)
	}
	return maxWait - elapsed
}

func (h *baseHandler) Shutdown() {
	h.done.Shutdown()
}

func (h *baseHandler) handleSessionAckMessage(message string) {
	ack, ok := parseSessionAckMessage(message)
	if !ok {
		h.log().Warn(h.server, "Unable to parse session acknowledgement", message)
		return
	}
	if ack.Error == "" && h.onSessionAcked != nil {
		h.onSessionAcked()
	}
	if h.sessionAcks == nil {
		return
	}

	select {
	case h.sessionAcks <- ack:
	case <-h.Done():
	default:
		h.log().Warn(h.server, "Dropping session acknowledgement because the queue is full", message)
	}
}

func parseSessionAckMessage(message string) (SessionAck, bool) {
	payload := strings.TrimSpace(message)
	if payload == "" {
		return SessionAck{}, false
	}

	switch {
	case strings.HasPrefix(payload, protocol.HiddenSessionStartOKPrefix):
		return parseSessionOKAck(strings.TrimPrefix(payload, protocol.HiddenSessionStartOKPrefix), "start")
	case strings.HasPrefix(payload, protocol.HiddenSessionUpdateOKPrefix):
		return parseSessionOKAck(strings.TrimPrefix(payload, protocol.HiddenSessionUpdateOKPrefix), "update")
	case strings.HasPrefix(payload, protocol.HiddenSessionErrorPrefix):
		return SessionAck{
			Action: "error",
			Error:  strings.TrimSpace(strings.TrimPrefix(payload, protocol.HiddenSessionErrorPrefix)),
		}, true
	default:
		return SessionAck{}, false
	}
}

func parseSessionOKAck(payload string, action string) (SessionAck, bool) {
	generationStr := strings.TrimSpace(payload)
	if generationStr == "" {
		return SessionAck{}, false
	}

	generation, err := strconv.ParseUint(generationStr, 10, 64)
	if err != nil {
		return SessionAck{}, false
	}

	return SessionAck{
		Action:     action,
		Generation: generation,
	}, true
}

// formatServerErrorMessage builds the "SERVER|<server>|ERROR|<message>" audit
// line shown to the user and written to the client log file. It carries NO
// trailing newline: it is emitted via the diagnostic (Log) sink, which appends
// the newline itself (adding one here would produce a blank line).
func formatServerErrorMessage(server string, message string) string {
	return protocol.EncodeServerError(server, message)
}
