package handlers

import (
	"io"
)

// Handler provides all methods which can be run on any client handler.
type Handler interface {
	io.ReadWriter
	SendMessage(command string) error
	Server() string
	Status() int
	Shutdown()
	Done() <-chan struct{}
}
