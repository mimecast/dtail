package handlers

import (
	"errors"
	"fmt"
	"time"
)

// HealthHandler implements the handler required for health checks.
type HealthHandler struct {
	withCancel
	// Buffer of incoming data from server.
	receiveBuf []byte
	// To send commands to the server.
	commands chan string
	// To receive messages from the server.
	receive chan<- string
	// The remote server address
	server string
	status int
}

// NewHealthHandler returns a new health check handler.
func NewHealthHandler(server string, receive chan<- string) *HealthHandler {
	h := HealthHandler{
		server:   server,
		receive:  receive,
		commands: make(chan string),
		status:   -1,
		withCancel: withCancel{
			done: make(chan struct{}),
		},
	}

	return &h
}

// Server returns the remote server name.
func (h *HealthHandler) Server() string {
	return h.server
}

// Status of the handler.
func (h *HealthHandler) Status() int {
	return h.status
}

// SendMessage sends a DTail command to the server.
func (h *HealthHandler) SendMessage(command string) error {
	select {
	case h.commands <- fmt.Sprintf("%s;", command):
	case <-time.NewTimer(time.Second * 10).C:
		return errors.New("Timed out sending command " + command)
	}

	return nil
}

// Server writes byte stream to client.
func (h *HealthHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		h.receiveBuf = append(h.receiveBuf, b)
		if b == '\n' {
			h.receive <- string(h.receiveBuf)
			h.receiveBuf = h.receiveBuf[:0]
		}
	}

	return len(p), nil
}

// Server reads byte stream from client.
func (h *HealthHandler) Read(p []byte) (n int, err error) {
	n = copy(p, []byte(<-h.commands))
	return
}
