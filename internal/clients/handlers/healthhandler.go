package handlers

import (
	"fmt"
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
			status:       -1,
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
			dlog.Client.Debug(message)
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
	status := strings.Split(message, ":")
	fmt.Println(status)
	/*
		switch status {
		case "OK":
			h.HealthStatusCh <- 0
		case "WARNING":
			h.HealthStatusCh <- 1
		case "CRITICAL":
			h.HealthStatusCh <- 2
		case "UNKNOWN":
			h.HealthStatusCh <- 3
		default:
			fmt.Println("CRITICAL: Unexpected server response: '%s'")
			h.HealthStatusCh <- 2
		}
	*/
}
