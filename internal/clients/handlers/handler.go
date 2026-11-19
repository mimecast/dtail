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
	WaitForSessionAck(timeout time.Duration) (SessionAck, bool)
}
