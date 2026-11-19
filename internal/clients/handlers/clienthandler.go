package handlers

import (
	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/clients/clientlog"
)

// ClientHandler is the basic client handler interface.
type ClientHandler struct {
	baseHandler
}

var _ Handler = (*ClientHandler)(nil)

// NewClientHandler creates a new client handler.
func NewClientHandler(server string, logger clientlog.Logger) *ClientHandler {
	logger = clientlog.OrNop(logger)
	logger.Debug(server, "Creating new client handler")

	return &ClientHandler{
		baseHandler{
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
	}
}
