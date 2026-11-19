package handlers

import (
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
	return &HealthHandler{
		baseHandler: baseHandler{
			server:         server,
			shellStarted:   false,
			commands:       make(chan string),
			status:         2, // Assume CRITICAL status by default.
			done:           internal.NewDone(),
			capabilities:   make(map[string]struct{}),
			capabilitiesCh: make(chan struct{}),
			sessionAcks:    make(chan SessionAck, 4),
			logger:         logger,
		},
	}
}

// Read data from the dtail server via Writer interface.
func (h *HealthHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n', protocol.MessageDelimiter:
			message := h.receiveBuf.String()
			h.handleMessage(message)
			h.receiveBuf.Reset()
		default:
			h.receiveBuf.WriteByte(b)
		}
	}
	return len(p), nil
}

func (h *HealthHandler) handleMessage(message string) {
	if len(message) > 0 && message[0] == '.' {
		h.handleHiddenMessage(message)
		return
	}
	decoded, err := protocol.DecodeMessage(message)
	if err == nil && decoded.Kind != protocol.MessagePlain {
		message = decoded.Content
	}
	if message == "OK" {
		h.status = 0
	}
}
