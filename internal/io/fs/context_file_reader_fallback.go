//go:build js || plan9 || wasip1

package fs

import (
	"context"
	"os"
)

// Platforms without descriptor readiness polling can still reject reads when
// cancellation is already visible. The borrowed descriptor remains untouched.
type contextFileReader struct {
	ctx  context.Context
	file *os.File
}

func newContextFileReader(ctx context.Context, file *os.File) (*contextFileReader, error) {
	return &contextFileReader{ctx: ctx, file: file}, nil
}

func (r *contextFileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}

func (r *contextFileReader) Close() error { return nil }
