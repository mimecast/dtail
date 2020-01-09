package handlers

import (
	"dtail/logger"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type baseHandler struct {
	server       string
	shellStarted bool
	commands     chan string
	pong         chan struct{}
	receiveBuf   []byte
	stop         chan struct{}
	pingTimeout  int
}

func (h *baseHandler) Server() string {
	return h.server
}

// Used to determine whether server is still responding to requests or not.
func (h *baseHandler) Ping() error {
	if h.pingTimeout == 0 {
		// Server ping disabled
		return nil
	}

	if err := h.SendCommand("ping"); err != nil {
		return err
	}

	select {
	case <-h.pong:
		return nil
	case <-time.After(time.Duration(h.pingTimeout) * time.Second):
	}

	return errors.New("Didn't receive any server pongs (ping replies)")
}

func (h *baseHandler) SendCommand(command string) error {
	if command == "ping" {
		logger.Trace("Sending command", h.server, command)
	} else {
		logger.Debug("Sending command", h.server, command)
	}

	select {
	case h.commands <- fmt.Sprintf("%s;", command):
	case <-time.After(time.Second * 5):
		return errors.New("Timed out sending command " + command)
	case <-h.stop:
	}

	return nil
}

// Read data from the dtail server via Writer interface.
func (h *baseHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		h.receiveBuf = append(h.receiveBuf, b)
		if b == '\n' {
			if len(h.receiveBuf) == 0 {
				continue
			}
			message := string(h.receiveBuf)
			h.handleMessageType(message)
		}
	}

	return len(p), nil
}

// Send data to the dtail server via Reader interface.
func (h *baseHandler) Read(p []byte) (n int, err error) {
	select {
	case command := <-h.commands:
		n = copy(p, []byte(command))
	case <-h.stop:
		return 0, io.EOF
	}
	return
}

// Handle various message types.
func (h *baseHandler) handleMessageType(message string) {
	if len(h.receiveBuf) == 0 {
		return
	}
	// Hidden server commands starti with a dot "."
	if h.receiveBuf[0] == '.' {
		h.handleHiddenMessage(message)
		h.receiveBuf = h.receiveBuf[:0]
		return
	}

	// Silent mode will only print out remote logs but not remote server
	// commands. But remote server commands will be still logged to ./log/.
	if logger.Mode == logger.SilentMode {
		if h.receiveBuf[0] == 'R' {
			logger.Raw(message)
		}
		h.receiveBuf = h.receiveBuf[:0]
		return
	}
	logger.Raw(message)
	h.receiveBuf = h.receiveBuf[:0]
}

// Handle messages received from server which are not meant to be displayed
// to the end user.
func (h *baseHandler) handleHiddenMessage(message string) {
	switch {
	case strings.HasPrefix(message, ".pong"):
		h.pong <- struct{}{}
	case strings.HasPrefix(message, ".syn close connection"):
		h.SendCommand("ack close connection")
	}
}

// Stop the handler.
func (h *baseHandler) Stop() {
	select {
	case <-h.stop:
	default:
		logger.Debug("Stopping base handler", h.server)
		close(h.stop)
	}
}
