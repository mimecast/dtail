package handlers

import (
	"github.com/mimecast/dtail/internal/logger"
)

// ClientHandler is the basic client handler interface.
type ClientHandler struct {
	baseHandler
}

// NewClientHandler creates a new client handler.
func NewClientHandler(server string, pingTimeout int) *ClientHandler {
	logger.Debug(server, "Creating new client handler")

	return &ClientHandler{
		baseHandler{
			server:       server,
			shellStarted: false,
			commands:     make(chan string),
			pong:         make(chan struct{}, 1),
			stop:         make(chan struct{}),
			pingTimeout:  pingTimeout,
		},
	}
}
