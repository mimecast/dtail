package handlers

import (
	"context"
	"io"
)

// Handler provides all methods which can be run on any client handler.
type Handler interface {
	io.ReadWriter
	SendMessage(command string) error
	Server() string
	Status() int
	WithCancel(ctx context.Context) (context.Context, context.CancelFunc)
	Done() <-chan struct{}
}
