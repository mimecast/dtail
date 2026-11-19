//go:build !linux

// Package journal provides a journalctl-backed file reader.
package journal

import (
	"context"
	"errors"
	"runtime"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// ErrUnsupported reports that journal reading is only available on Linux.
var ErrUnsupported = errors.New("journal reader is only supported on linux")

// Reader is an unsupported journal reader stub on non-Linux systems.
type Reader struct{}

// NewReader returns an unsupported error on non-Linux systems.
func NewReader(_ []string, _ string, _ bool, _ chan<- string) (*Reader, error) {
	return nil, errors.Join(ErrUnsupported, errors.New(runtime.GOOS))
}

// Start returns an unsupported error on non-Linux systems.
func (r *Reader) Start(context.Context, lcontext.LContext, line.Processor, regex.Regex) error {
	return ErrUnsupported
}

// FilePath returns the journalctl command name.
func (r *Reader) FilePath() string {
	return "journalctl"
}

// Retry returns false on non-Linux systems.
func (r *Reader) Retry() bool {
	return false
}
