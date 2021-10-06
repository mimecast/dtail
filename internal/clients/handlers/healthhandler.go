package handlers

import (
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/protocol"
)

// HealthHandler is the handler used on the client side for running mapreduce aggregations.
type HealthHandler struct {
	baseHandler
}

// NewHealthHandler returns a new health client handler.
func NewHealthHandler(server string) *HealthHandler {
	dlog.Client.Debug(server, "Creating new health handler")
	return &HealthHandler{
		baseHandler: baseHandler{
			server:       server,
			shellStarted: false,
			commands:     make(chan string),
			status:       2, // Assume CRITICAL status by default.
			done:         internal.NewDone(),
		},
	}
}

// Read data from the dtail server via Writer interface.
func (h *HealthHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n', protocol.MessageDelimiter:
			message := h.baseHandler.receiveBuf.String()
			h.handleMessage(message)
			h.baseHandler.receiveBuf.Reset()
		default:
			h.baseHandler.receiveBuf.WriteByte(b)
		}
	}
	return len(p), nil
}

func (h *HealthHandler) handleMessage(message string) {
	if len(message) > 0 && message[0] == '.' {
		h.baseHandler.handleHiddenMessage(message)
		return
	}
	s := strings.Split(message, protocol.FieldDelimiter)
	message = s[len(s)-1]
	if message == "OK" {
		h.baseHandler.status = 0
	}
}
