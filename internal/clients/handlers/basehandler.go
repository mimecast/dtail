package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
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
	// frameUnmarked is a frame of a session that does not mark control
	// frames (protocol.Framing.MarksControl): its content tells whether it
	// is a control message.
	frameUnmarked frameKind = iota
	// framePayload is an unmarked frame of a session that marks control
	// frames: it is never a control message, whatever its content.
	framePayload
	// frameControl is a marked control frame of such a session.
	frameControl
)

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
	// (single output-copy goroutine), and by requestFraming before the
	// handler is used.
	pendingCommand []byte
	// lineBuf is scratch space for appending the newline to a payload message
	// that lacks one. It is released after a message that grew it beyond
	// maxRetainedLineBufBytes. Only touched by Write (single input-copy
	// goroutine).
	lineBuf []byte
	// frameBuf is scratch space for decoding an escaped frame (see
	// decodeFrame); released like lineBuf. Only touched by Write.
	frameBuf []byte
	status   int

	// framingRequested is set when the handler sends
	// protocol.EscapedFramingCommand and protocol.ControlFramesCommand as
	// its first two frames. framingState holds the protocol.Framing of every
	// frame after the session's first one, decided from that first frame
	// (see decideFraming): RawFraming, EscapedFraming when it is the server's
	// capabilities frame listing protocol.CapabilityEscapedFramingV1, or
	// EscapedControlFraming when it also lists
	// protocol.CapabilityControlFramesV1. framingDecided is set once the
	// first frame was seen, so no later frame changes the framing (a later
	// frame may well be a log line of a raw session that happens to read like
	// a capabilities frame). framingDecided is only touched by Write.
	framingRequested bool
	framingState     atomic.Uint32
	framingDecided   bool

	capabilitiesMu sync.RWMutex
	capabilities   map[string]struct{}
	capabilitiesCh chan struct{}
	capabilitiesOk sync.Once

	sessionAcks chan SessionAck
	logger      clientlog.Logger

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
	// first report. Only servers advertising
	// protocol.CapabilityCommandFailureV1 report failures. Any failure makes
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
		h.status,
		h,
	)
}

func (h *baseHandler) Server() string {
	return h.server
}

func (h *baseHandler) Status() int {
	return h.status
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

func (h *baseHandler) ReportServerError(message string) {
	if h.status <= 0 {
		h.status = 1
	}
	// Route through the DIAGNOSTIC (Log) sink, not Raw: a server-error report is
	// an audit line, not bulk payload. Via Raw it would be gated out of the
	// client log file whenever Client.LogPayload is false (the default), silently
	// dropping the error from the on-disk audit trail. RawLog keeps it in the
	// file like other diagnostics while still printing it to stdout. The message
	// carries no trailing newline; the Log sink appends one.
	clientlog.RawDiagnostic(h.log(), formatServerErrorMessage(h.server, message))
}

// SendMessage to the server.
func (h *baseHandler) SendMessage(command string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	// A scheduled job's read share stays out of the log, in the command and
	// in its encoding.
	loggedCommand, loggedEncoded := command, encoded
	if redacted := config.RedactReadShare(command); redacted != command {
		loggedCommand, loggedEncoded = redacted, "(redacted)"
	}
	h.log().Debug("Sending command", h.server, loggedCommand, loggedEncoded)

	select {
	case h.commands <- commandFrame(encoded):
	case <-time.After(time.Second * 5):
		return fmt.Errorf("timed out sending command '%s' (base64: '%s')", loggedCommand, loggedEncoded)
	case <-h.Done():
		return nil
	}

	return nil
}

// requestFraming makes protocol.EscapedFramingCommand and
// protocol.ControlFramesCommand the first two frames Read sends, in one
// piece, ahead of every command passed to SendMessage (e.g. AUTHKEY and the
// read commands), whichever connector drives the handler. Servers that do not
// advertise protocol.CapabilityEscapedFramingV1 may close on these unknown
// commands before reading the workload; decideFraming reports them as unsupported.
// The session then keeps raw framing; servers that advertise escaped framing but not
// protocol.CapabilityControlFramesV1 ignore the second one, so the session is
// escaped without marked control frames. It must be called before the
// handler is used.
func (h *baseHandler) requestFraming() {
	h.framingRequested = true
	h.pendingCommand = []byte(
		commandFrame(base64.StdEncoding.EncodeToString([]byte(protocol.EscapedFramingCommand))) +
			commandFrame(base64.StdEncoding.EncodeToString([]byte(protocol.ControlFramesCommand))))
}

// commandFrame frames a base64-encoded command for the server.
func commandFrame(encodedCommand string) string {
	return fmt.Sprintf("protocol %s base64 %v;", protocol.ProtocolCompat, encodedCommand)
}

// framing returns the framing of the frames received from now on.
func (h *baseHandler) framing() protocol.Framing {
	return protocol.Framing(h.framingState.Load())
}

// decodeFrame returns the body of a received frame (without its delimiter)
// in the session framing, and how to handle it (see frameKind). The result
// aliases frame or frameBuf and is only valid until the next decodeFrame
// call; frame itself is never modified. The session's first frame decides
// the framing of all later ones (see decideFraming). The control frame
// marker is cut off before the frame is decoded, as a decoded body may start
// with the same bytes.
func (h *baseHandler) decodeFrame(frame []byte) ([]byte, frameKind) {
	framing := h.framing()
	kind := frameUnmarked
	if framing.MarksControl() {
		var control bool
		frame, control = framing.CutControl(frame)
		kind = framePayload
		if control {
			kind = frameControl
		}
	}
	decoded := framing.DecodeFrame(frame, &h.frameBuf)
	if cap(h.frameBuf) > maxRetainedLineBufBytes {
		// decoded stays valid: only the handler's reference is dropped, so
		// an oversized frame does not pin its buffer (same policy as lineBuf).
		h.frameBuf = nil
	}
	if !h.framingDecided {
		h.framingDecided = true
		h.decideFraming(decoded)
	}
	return decoded, kind
}

// decideFraming switches the session to escaped framing when the session
// asked for it and its first frame, which is always raw, is the server's
// capabilities frame listing protocol.CapabilityEscapedFramingV1, and on to
// marked control frames (protocol.EscapedControlFraming) when that frame
// also lists protocol.CapabilityControlFramesV1. Such a server sends that
// frame first and frames every later frame so; this runs on the Write
// goroutine before the next frame is decoded. Any other first frame keeps
// raw framing for good: a server without the capability, or one that sends
// something else first, never escapes.
func (h *baseHandler) decideFraming(first []byte) {
	if !h.framingRequested {
		return
	}
	capabilities, ok := bytes.CutPrefix(first, []byte(protocol.HiddenCapabilitiesPrefix))
	fields := strings.Fields(string(capabilities))
	if !ok || !slices.Contains(fields, protocol.CapabilityEscapedFramingV1) {
		// Older protocol-4.1 servers account unknown .ack commands as work.
		// Either a framing request or an input-batch marker can therefore
		// finish the session before its reads, even with a normal close
		// handshake. Never let that look like a successful empty query.
		h.ReportServerError("unsupported DTail server: missing capability " +
			protocol.CapabilityEscapedFramingV1 + "; please update DTail server")
		return
	}
	switch {
	case slices.Contains(fields, protocol.CapabilityControlFramesV1):
		h.framingState.Store(uint32(protocol.EscapedControlFraming))
	default:
		h.framingState.Store(uint32(protocol.EscapedFraming))
	}
}

// Write receives data from the dtail server via the io.Writer interface.
//
// The stream is a sequence of messages terminated by protocol.MessageDelimiter;
// newlines are ordinary payload bytes. Each message is decoded in the session
// framing (decodeFrame) before it is handled; under escaped framing no
// payload byte equals the delimiter, so a line containing e.g. '€' (whose
// UTF-8 encoding ends in the delimiter byte 0xAC) stays one message. The
// frames are cut out of the stream by scanFrames. handleMessage and
// everything below it must not retain the slice (io.Writer forbids retaining
// p).
func (h *baseHandler) Write(p []byte) (n int, err error) {
	h.scanFrames(p, h.handleFrame)
	return len(p), nil
}

// handleFrame decodes and handles one received frame.
func (h *baseHandler) handleFrame(frame []byte) {
	h.handleMessage(h.decodeFrame(frame))
}

// scanFrames calls handle with every frame that p completes, without its
// protocol.MessageDelimiter and not yet decoded (decodeFrame), in stream
// order. p is scanned with bytes.IndexByte instead of byte by byte; under
// escaped framing the delimiter byte only ever ends a frame, so an escape
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

// handleMessage routes one decoded message of the given kind. Control
// messages (hidden messages and AUTHKEY acknowledgements) are consumed here;
// all other messages are user payload and are written with exactly one
// trailing newline (writePayload). Which messages are control messages
// depends on the session framing: a marked control frame always is one, an
// unmarked frame of a session that marks control frames never is (so a
// --plain log line such as ".syn close connection" is printed, not acted
// upon), and in any other session the content tells (handleUnmarkedControl).
// message is only valid for the duration of the call.
func (h *baseHandler) handleMessage(message []byte, kind frameKind) {
	switch kind {
	case frameControl:
		h.handleControlMessage(message)
		return
	case frameUnmarked:
		if h.handleUnmarkedControl(message) {
			return
		}
	}
	h.writePayload(message)
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

// handleUnmarkedControl handles message as a control message, and reports
// true, when its content reads like one: it starts with '.' (a hidden
// message) or is an AUTHKEY acknowledgement, bare or in a SERVER or
// AGGREGATE frame. This is the routing of sessions that do not mark control
// frames (with older servers, or of older clients), in which a --plain log
// line reading like that is taken for a control message as well.
func (h *baseHandler) handleUnmarkedControl(message []byte) bool {
	if len(message) > 0 && message[0] == '.' {
		h.handleHiddenMessage(string(message))
		return true
	}
	return mayBeAuthKeyMessage(message) && h.handleAuthKeyMessage(string(message))
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

// authKeyPrefixes are the leading bytes a whitespace-trimmed message must start
// with for parseAuthKeyMessage to recognise it: a bare acknowledgement, or one
// wrapped in a tagged frame whose content is decoded first.
var authKeyPrefixes = [][]byte{
	[]byte("AUTHKEY"),
	[]byte(protocol.ServerMessageID + protocol.FieldSeparator()),
	[]byte(protocol.AggregateMessageID + protocol.FieldSeparator()),
}

// mayBeAuthKeyMessage is an allocation-free pre-check run on every payload
// message. It returns false only for messages parseAuthKeyMessage would reject,
// so ordinary log lines skip the string conversion, trimming and decoding.
func mayBeAuthKeyMessage(message []byte) bool {
	trimmed := bytes.TrimSpace(message)
	for _, prefix := range authKeyPrefixes {
		if bytes.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func parseAuthKeyMessage(message string) (isAuthKeyMessage bool, ok bool, detail string) {
	if message == "" {
		return false, false, ""
	}

	payload := strings.TrimSpace(message)
	decoded, err := protocol.DecodeMessage(payload)
	if err == nil && decoded.Kind != protocol.MessagePlain {
		payload = strings.TrimSpace(decoded.Content)
	}

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
	// The session framing is not decided here but from the session's first
	// frame only (see decideFraming).

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

// WaitForSessionAck returns the next session acknowledgement, waiting up to
// timeout for one. Like WaitForCapabilities, an acknowledgement that is
// already queued wins without any wait: the non-blocking receive comes first
// because a select with several ready cases picks one at random, so a queued
// acknowledgement could otherwise lose against an expired timer or an already
// finished session.
func (h *baseHandler) WaitForSessionAck(timeout time.Duration) (SessionAck, bool) {
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

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ack := <-h.sessionAcks:
		return ack, true
	case <-h.Done():
		return SessionAck{}, false
	case <-timer.C:
		return SessionAck{}, false
	}
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
