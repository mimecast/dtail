package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/protocol"
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
	// (single output-copy goroutine).
	pendingCommand []byte
	status         int

	capabilitiesMu sync.RWMutex
	capabilities   map[string]struct{}
	capabilitiesCh chan struct{}
	capabilitiesOk sync.Once

	sessionAcks chan SessionAck
	logger      clientlog.Logger
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
	h.log().Debug("Sending command", h.server, command, encoded)

	select {
	case h.commands <- fmt.Sprintf("protocol %s base64 %v;", protocol.ProtocolCompat, encoded):
	case <-time.After(time.Second * 5):
		return fmt.Errorf("timed out sending command '%s' (base64: '%s')", command, encoded)
	case <-h.Done():
		return nil
	}

	return nil
}

// Read data from the dtail server via Writer interface.
func (h *baseHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n':
			// Just add the newline to the buffer, don't treat as message delimiter
			h.receiveBuf.WriteByte(b)
		case protocol.MessageDelimiter:
			message := h.receiveBuf.String()
			h.handleMessage(message)
			h.receiveBuf.Reset()
		default:
			h.receiveBuf.WriteByte(b)
		}
	}
	return len(p), nil
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

func (h *baseHandler) handleMessage(message string) {
	if len(message) > 0 && message[0] == '.' {
		h.handleHiddenMessage(message)
		return
	}
	if h.handleAuthKeyMessage(message) {
		return
	}

	// Add newline only if the message doesn't already end with one
	if len(message) > 0 && message[len(message)-1] == '\n' {
		clientlog.Raw(h.log(), message)
	} else {
		clientlog.Raw(h.log(), message+"\n")
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
	case strings.HasPrefix(message, ".syn close connection"):
		if err := h.SendMessage(".ack close connection"); err != nil {
			h.log().Debug(h.server, "Unable to acknowledge close connection", err)
		}
		h.Shutdown()
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

func (h *baseHandler) WaitForCapabilities(timeout time.Duration) bool {
	if h.capabilitiesCh == nil {
		return false
	}

	if timeout <= 0 {
		select {
		case <-h.capabilitiesCh:
			return true
		default:
			return false
		}
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

func (h *baseHandler) WaitForSessionAck(timeout time.Duration) (SessionAck, bool) {
	if h.sessionAcks == nil {
		return SessionAck{}, false
	}

	if timeout <= 0 {
		select {
		case ack := <-h.sessionAcks:
			return ack, true
		default:
			return SessionAck{}, false
		}
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
	var encoded bytes.Buffer
	protocol.EncodeMessage(&encoded, protocol.Message{
		Kind:     protocol.MessageServer,
		Hostname: server,
		Content:  "ERROR" + protocol.FieldDelimiter + message,
	})
	frame := encoded.Bytes()
	return string(frame[:len(frame)-1])
}
