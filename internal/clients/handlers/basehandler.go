package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/protocol"
)

type baseHandler struct {
	done         *internal.Done
	server       string
	shellStarted bool
	commands     chan string
	receiveBuf   bytes.Buffer
	status       int
}

func (h *baseHandler) String() string {
	return fmt.Sprintf("baseHandler(%s,server:%s,shellStarted:%v,status:%d)@%p",
		h.done,
		h.server,
		h.shellStarted,
		h.status,
		h,
	)
}

func (h *baseHandler) Server() string {
	return h.server
}

func (h *baseHandler) Status() int {
	return h.status
}

func (h *baseHandler) Done() <-chan struct{} {
	return h.done.Done()
}

func (h *baseHandler) Shutdown() {
	h.done.Shutdown()
}

// SendMessage to the server.
func (h *baseHandler) SendMessage(command string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	dlog.Client.Debug("Sending command", h.server, command, encoded)

	select {
	case h.commands <- fmt.Sprintf("protocol %s base64 %v;", protocol.ProtocolCompat, encoded):
	case <-time.After(time.Second * 5):
		return fmt.Errorf("Timed out sending command '%s' (base64: '%s')", command, encoded)
	case <-h.Done():
		return nil
	}

	return nil
}

// Read data from the dtail server via Writer interface.
func (h *baseHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		/*
			// NEXT: Next DTail version make it so that '\n' gets ignored. For now
			// leave it for compatibility with older DTail server + ability to display
			// the protocol mismatch warn message.
			case '\n' {
				continue
		*/
		case '\n', protocol.MessageDelimiter:
			message := h.receiveBuf.String()
			/*
				// dcat/grep should actually display empty lines.
					if len(message) == 0 {
						continue
					}
			*/
			h.handleMessageType(message)
			h.receiveBuf.Reset()
		default:
			h.receiveBuf.WriteByte(b)
		}
	}

	return len(p), nil
}

// Send data to the dtail server via Reader interface.
func (h *baseHandler) Read(p []byte) (n int, err error) {
	select {
	case command := <-h.commands:
		n = copy(p, []byte(command))
	case <-h.Done():
		return 0, io.EOF
	}
	return
}

// Handle various message types.
func (h *baseHandler) handleMessageType(message string) {
	// Hidden server commands starti with a dot "."
	if len(message) > 0 && message[0] == '.' {
		h.handleHiddenMessage(message)
		return
	}

	dlog.Client.Raw(message)
}

// Handle messages received from server which are not meant to be displayed
// to the end user.
func (h *baseHandler) handleHiddenMessage(message string) {
	switch {
	case strings.HasPrefix(message, ".syn close connection"):
		h.SendMessage(".ack close connection")
	}
}
