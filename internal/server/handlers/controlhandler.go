package handlers

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal/io/logger"
	user "github.com/mimecast/dtail/internal/user/server"
)

// ControlHandler is used for control functions and health monitoring.
type ControlHandler struct {
	ctx            context.Context
	done           chan struct{}
	hostname       string
	payload        []byte
	serverMessages chan string
	user           *user.User
}

// NewControlHandler returns a new control handler.
func NewControlHandler(ctx context.Context, user *user.User) (*ControlHandler, <-chan struct{}) {
	logger.Debug(user, "Creating control handler")

	h := ControlHandler{
		ctx:            ctx,
		done:           make(chan struct{}),
		serverMessages: make(chan string, 10),
		user:           user,
	}

	fqdn, err := os.Hostname()
	if err != nil {
		logger.FatalExit(err)
	}

	s := strings.Split(fqdn, ".")
	h.hostname = s[0]

	return &h, h.done
}

// Read is to send data to the client via the Reader interface.
func (h *ControlHandler) Read(p []byte) (n int, err error) {
	for {
		select {
		case message := <-h.serverMessages:
			wholePayload := []byte(fmt.Sprintf("SERVER|%s|%s\n", h.hostname, message))
			n = copy(p, wholePayload)
			return
		case <-h.ctx.Done():
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
			h.handleCommand(h.ctx, wholePayload)
			h.payload = nil

		default:
			h.payload = append(h.payload, c)
		}
	}

	n = len(p)
	return
}

func (h *ControlHandler) handleCommand(ctx context.Context, command string) {
	logger.Info(h.user, command)
	s := strings.Split(command, " ")
	logger.Debug(h.user, "Receiving command", command, s)

	switch s[0] {
	case "health":
		h.serverMessages <- "OK: DTail SSH Server seems fine"
		h.serverMessages <- "done;"
	case "debug":
		h.serverMessages <- logger.Debug(h.user, "Receiving debug command", command, s)
	default:
		h.serverMessages <- logger.Warn(h.user, "Received unknown control command", command, s)
	}
}
