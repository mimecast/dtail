package handlers

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal/logger"
	user "github.com/mimecast/dtail/internal/user/server"
)

// ControlHandler is used for control functions and health monitoring.
type ControlHandler struct {
	serverMessages chan string
	pong           chan struct{}
	stop           chan struct{}
	payload        []byte
	hostname       string
	user           *user.User
}

// NewControlHandler returns a new control handler.
func NewControlHandler(user *user.User) *ControlHandler {
	logger.Debug(user, "Creating control handler")

	h := ControlHandler{
		serverMessages: make(chan string, 10),
		pong:           make(chan struct{}, 10),
		stop:           make(chan struct{}),
		user:           user,
	}

	fqdn, err := os.Hostname()
	if err != nil {
		logger.FatalExit(err)
	}

	s := strings.Split(fqdn, ".")
	h.hostname = s[0]
	return &h
}

// Read is to send data to the client via the Reader interface.
func (h *ControlHandler) Read(p []byte) (n int, err error) {
	for {
		select {
		case message := <-h.serverMessages:
			wholePayload := []byte(fmt.Sprintf("SERVER|%s|%s\n", h.hostname, message))
			n = copy(p, wholePayload)
			return
		case <-h.pong:
			logger.Info(h.user, "Sending pong")
			n = copy(p, []byte(".pong\n"))
			return
		case <-h.stop:
			return 0, io.EOF
		}
	}
}

// Write is to read data to the client via the Writer interface.
func (h *ControlHandler) Write(p []byte) (n int, err error) {
	for _, c := range p {
		switch c {
		case ';':
			wholePayload := strings.TrimSpace(string(h.payload))
			h.handleCommand(wholePayload)
			h.payload = nil

		default:
			h.payload = append(h.payload, c)
		}
	}

	n = len(p)
	return
}

// Close the control handler.
func (h *ControlHandler) Close() {
	close(h.stop)
}

// Wait returns the handler stop channel.
func (h *ControlHandler) Wait() <-chan struct{} {
	return h.stop
}

func (h *ControlHandler) handleCommand(command string) {
	logger.Info(h.user, command)
	s := strings.Split(command, " ")
	logger.Debug(h.user, "Receiving command", command, s)

	switch s[0] {
	case "health":
		h.serverMessages <- "OK: DTail SSH Server seems fine"
		h.serverMessages <- "done;"
	case "ping":
		h.pong <- struct{}{}
	case "debug":
		h.serverMessages <- logger.Debug(h.user, "Receiving debug command", command, s)
	default:
		h.serverMessages <- logger.Warn(h.user, "Received unknown command", command, s)
	}
}
