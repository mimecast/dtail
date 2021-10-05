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
	HealthStatusCh chan<- int
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
		HealthStatusCh: make(chan int),
	}
}

// Read data from the dtail server via Writer interface.
func (h *HealthHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n':
			continue
		case protocol.MessageDelimiter:
			message := h.baseHandler.receiveBuf.String()
			dlog.Client.Debug(message)
			h.handleHealthMessage(message)
			h.baseHandler.receiveBuf.Reset()
		default:
			h.baseHandler.receiveBuf.WriteByte(b)
		}
	}

	return len(p), nil
}

func (h *HealthHandler) handleHealthMessage(message string) {
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
