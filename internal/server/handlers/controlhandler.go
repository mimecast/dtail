package handlers

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/user/server"
)

// ControlHandler is used for control functions and health monitoring.
type ControlHandler struct {
	done           *internal.Done
	hostname       string
	payload        []byte
	serverMessages chan string
	user           *user.User
}

// NewControlHandler returns a new control handler.
func NewControlHandler(user *user.User) *ControlHandler {
	logger.Debug(user, "Creating control handler")

	h := ControlHandler{
		done:           internal.NewDone(),
		serverMessages: make(chan string, 10),
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

// Shutdown the handler.
func (h *ControlHandler) Shutdown() {
	h.done.Shutdown()
}

// Done channel of the handler.
func (h *ControlHandler) Done() <-chan struct{} {
	return h.done.Done()
}

// Read is to send data to the client via the Reader interface.
func (h *ControlHandler) Read(p []byte) (n int, err error) {
	for {
		select {
		case message := <-h.serverMessages:
			wholePayload := []byte(fmt.Sprintf("SERVER|%s|%s%b", h.hostname, message, protocol.MessageDelimiter))
			n = copy(p, wholePayload)
			return
		case <-h.done.Done():
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

func (h *ControlHandler) handleCommand(command string) {
	logger.Info(h.user, command)
	s := strings.Split(command, " ")
	logger.Debug(h.user, "Receiving command", command, s)

	switch s[0] {
	case "health":
		h.serverMessages <- "OK: DTail SSH Server seems fine"
		h.serverMessages <- "done;"
	default:
		h.serverMessages <- logger.Error(h.user, "Received unknown control command", command, s)
	}
}
