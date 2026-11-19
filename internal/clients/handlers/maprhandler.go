package handlers

import (
	"bytes"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// newline is the byte MaprHandler drops from every frame.
var newline = []byte{'\n'}

// MaprHandler is the handler used on the client side for running mapreduce
// aggregations.
type MaprHandler struct {
	baseHandler
	aggregate *client.Aggregate
	// newlineBuf is scratch space for a frame without its newlines (see
	// dropNewlines); released like lineBuf. Only touched by Write.
	newlineBuf []byte
}

// NewMaprHandler returns a new mapreduce client handler.
func NewMaprHandler(server string, session *client.SessionState, logger clientlog.Logger) *MaprHandler {
	logger = clientlog.OrNop(logger)
	h := &MaprHandler{
		baseHandler: baseHandler{
			server:         server,
			shellStarted:   false,
			commands:       make(chan string),
			status:         -1,
			done:           internal.NewDone(),
			capabilities:   make(map[string]struct{}),
			capabilitiesCh: make(chan struct{}),
			sessionAcks:    make(chan SessionAck, 4),
			logger:         logger,
		},
		aggregate: client.NewAggregate(server, session, logger),
	}
	h.requestFraming()
	return h
}

// Write receives data from the dtail server via the io.Writer interface.
//
// The frames are cut out of p by scanFrames (bulk delimiter scanning, frames
// split across Write calls kept in receiveBuf). Every newline byte of a frame
// is dropped before the frame is decoded, so an aggregate frame's trailing
// newline does not reach the aggregate parser; a non-aggregate message gets
// exactly one trailing newline back from handleMessage. No escape sequence of
// the session framing contains a newline byte, so for a well-formed frame
// dropping it before decoding is the same as dropping it after. Only a
// malformed frame tells them apart: FF 0A 01 (an escape byte, a newline, then
// an escape code) decodes to 0xAC when the newline is dropped first, but is
// kept verbatim as a malformed sequence and becomes FF 01 when it is dropped
// after. No encoder produces that sequence (the escape byte is always
// followed by its code directly), and the handler before the batched receive
// also skipped newlines while accumulating a frame, before decoding it, so
// this is no change in behaviour. Each frame is decoded in the
// session framing (see baseHandler.decodeFrame), so group keys and values
// containing e.g. '€' are not cut at their 0xAC byte.
//
// The aggregate messages of p are queued and merged into the global group set
// once, after the whole of p was handled (see client.Aggregate).
func (h *MaprHandler) Write(p []byte) (n int, err error) {
	h.scanFrames(p, h.handleMaprFrame)
	if err := h.aggregate.Merge(); err != nil {
		h.log().Error("Unable to aggregate data", h.server, err)
	}
	return len(p), nil
}

// handleMaprFrame handles one received frame, still undecoded. A frame that
// is empty once its newlines are dropped is ignored. A marked control frame
// is never aggregate data.
func (h *MaprHandler) handleMaprFrame(frame []byte) {
	frame = h.dropNewlines(frame)
	if len(frame) == 0 {
		return
	}
	message, kind := h.decodeFrame(frame)
	if logging.DebugEnabled(h.log()) {
		h.log().Debug(string(message))
	}
	if kind != frameControl {
		if content, isAggregate, decodeErr := protocol.CutAggregateContent(message); isAggregate {
			h.handleAggregateMessage(message, content, decodeErr)
			return
		}
	}
	h.handleMessage(message, kind)
}

// dropNewlines returns frame without its newline bytes. A frame without any
// is returned as it is; otherwise the result is built in newlineBuf and only
// valid until the next call. frame itself is never modified (it may alias
// Write's p).
func (h *MaprHandler) dropNewlines(frame []byte) []byte {
	if bytes.IndexByte(frame, '\n') < 0 {
		return frame
	}
	stripped := h.newlineBuf[:0]
	for len(frame) > 0 {
		before, after, _ := bytes.Cut(frame, newline)
		stripped = append(stripped, before...)
		frame = after
	}
	h.newlineBuf = stripped
	if cap(h.newlineBuf) > maxRetainedLineBufBytes {
		// stripped stays valid; only the handler's reference is dropped (same
		// policy as lineBuf and frameBuf).
		h.newlineBuf = nil
	}
	return stripped
}

// isAggregateMessage reports whether a wire message starts with a mapreduce
// aggregate tag. Valid frames are handed to the aggregate parser; malformed
// tagged frames are reported as protocol errors. Matching the full
// AggregateMessageID field prefix, rather than just the first byte 'A', keeps
// plain-mode protocol acks such as "AUTHKEY OK" out of the parser; those would
// otherwise trigger a spurious "Unable to aggregate data ... expected 3
// parts" error. Non-aggregate messages are routed to the base handler, which
// recognises acks as control. handleMaprFrame classifies with the same
// protocol.CutAggregateContent.
func isAggregateMessage(message string) bool {
	_, isAggregate, _ := protocol.CutAggregateContent([]byte(message))
	return isAggregate
}

// handleAggregateMessage queues the content of an aggregate message for the
// merge at the end of Write. message is the whole decoded frame, for the
// error log.
func (h *MaprHandler) handleAggregateMessage(message, content []byte, decodeErr error) {
	if decodeErr != nil {
		h.log().Error("Unable to decode aggregate data", h.server, string(message), decodeErr)
		return
	}
	if err := h.aggregate.Add(content); err != nil {
		h.log().Error("Unable to aggregate data", h.server, string(message), err)
	}
}

// Shutdown flushes any pending aggregate state before marking the handler done.
func (h *MaprHandler) Shutdown() {
	if h.aggregate != nil {
		if err := h.aggregate.Flush(); err != nil {
			h.log().Error("Unable to flush aggregate data on shutdown", h.server, err)
		}
	}
	h.baseHandler.Shutdown()
}
