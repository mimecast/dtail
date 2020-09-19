package handlers

import (
	"github.com/mimecast/dtail/internal/io/logger"
)

// ClientHandler is the basic client handler interface.
type ClientHandler struct {
	baseHandler
}

// NewClientHandler creates a new client handler.
func NewClientHandler(server string) *ClientHandler {
	logger.Debug(server, "Creating new client handler")

	return &ClientHandler{
		baseHandler{
			server:       server,
			shellStarted: false,
			commands:     make(chan string),
			status:       -1,
			done:         NewDone(),
		},
	}
}
