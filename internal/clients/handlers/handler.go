package handlers

import (
	"io"
	"time"
)

// Handler provides all methods which can be run on any client handler.
type Handler interface {
	io.ReadWriter
	Capabilities() []string
	HasCapability(name string) bool
	ReportServerError(message string)
	SendMessage(command string) error
	Server() string
	Status() int
	Shutdown()
	Done() <-chan struct{}
	WaitForCapabilities(timeout time.Duration) bool
	// WaitForSessionAck waits for the next session acknowledgement until the
	// handler was idle for timeout, at most maxWait (wall clock, 0: no cap).
	WaitForSessionAck(timeout, maxWait time.Duration) (SessionAck, bool)
}
