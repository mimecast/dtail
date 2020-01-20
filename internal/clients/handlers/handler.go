package handlers

import "io"

// Handler provides all methods which can be run on any client handler.
type Handler interface {
	io.ReadWriter
	Ping() error
	Stop()
	SendCommand(command string) error
	Server() string
}
