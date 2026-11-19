package handlers

import (
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/protocol"
)

// HealthHandler is the handler used on the client side for running mapreduce
// aggregations.
type HealthHandler struct {
	baseHandler
}

// NewHealthHandler returns a new health client handler.
func NewHealthHandler(server string, logger clientlog.Logger) *HealthHandler {
	logger = clientlog.OrNop(logger)
	logger.Debug(server, "Creating new health handler")
	h := &HealthHandler{
		baseHandler: baseHandler{
			server:         server,
			shellStarted:   false,
			commands:       make(chan string),
			done:           internal.NewDone(),
			capabilities:   make(map[string]struct{}),
			capabilitiesCh: make(chan struct{}),
			sessionAcks:    make(chan SessionAck, 4),
			logger:         logger,
		},
	}
	h.status.Store(2) // Assume CRITICAL status by default.
	return h
}

// Write receives data from the dtail server via the io.Writer interface. The
// frames are cut out of p and decoded like those of every other session
// (scanFrames, decodeFrame): the server's health handler frames them in
// protocol.EscapedControlFraming too, but sends no capabilities frame, so the
// first frame is not checked (expectCapabilities is unset). Marked control
// frames, such as the close handshake, are handled as hidden messages; every
// other frame is checked for the health status (handleMessage).
func (h *HealthHandler) Write(p []byte) (n int, err error) {
	// Measured as busy time for WaitForSessionAck (busyClock), the
	// whole call.
	h.busy.begin()
	defer h.busy.end()
	h.scanFrames(p, h.handleHealthFrame)
	return len(p), nil
}

// handleHealthFrame decodes and handles one received frame.
func (h *HealthHandler) handleHealthFrame(frame []byte) {
	message, kind := h.decodeFrame(frame)
	if kind == frameControl {
		h.handleControlMessage(message)
		return
	}
	h.handleMessage(string(message))
}

// handleMessage sets the status to OK (0) when message, a payload frame, is
// the server's "OK", bare or in a SERVER frame, with or without a trailing
// newline.
func (h *HealthHandler) handleMessage(message string) {
	decoded, err := protocol.DecodeMessage(message)
	if err == nil && decoded.Kind != protocol.MessagePlain {
		message = decoded.Content
	}
	if strings.TrimRight(message, "\n") == "OK" {
		h.status.Store(0)
	}
}
