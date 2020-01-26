package handlers

import "io"

// Handler interface for server side functionality.
type Handler interface {
	io.ReadWriter
}
