package handlers

import (
	"errors"
	"fmt"
	"time"
)

// HealthHandler implements the handler required for health checks.
type HealthHandler struct {
	// Buffer of incoming data from server.
	receiveBuf []byte
	// To send commands to the server.
	commands chan string
	// To receive messages from the server.
	receive chan<- string
	// The remote server address
	server string
}

// NewHealthHandler returns a new health check handler.
func NewHealthHandler(server string, receive chan<- string) *HealthHandler {
	h := HealthHandler{
		server:   server,
		receive:  receive,
		commands: make(chan string),
	}

	return &h
}

// Server returns the remote server name.
func (h *HealthHandler) Server() string {
	return h.server
}

// Stop is not of use for health check handler.
func (h *HealthHandler) Stop() {
	// Nothing done here.
}

// Ping is not of use for health check handler.
func (h *HealthHandler) Ping() error {
	return nil
}

// SendCommand send a DTail command to the server.
func (h *HealthHandler) SendCommand(command string) error {
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
