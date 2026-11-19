//go:build windows

package fs

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// contextFileReader reads from an owned duplicate of the caller's handle.
// Closing that duplicate cancels pending pipe I/O on Windows while preserving
// the borrowed os.Stdin handle and its flags for the caller.
type contextFileReader struct {
	ctx      context.Context
	file     *os.File
	done     chan struct{}
	closer   sync.Once
	closeErr error
}

func newContextFileReader(ctx context.Context, file *os.File) (*contextFileReader, error) {
	process := windows.CurrentProcess()
	var handle windows.Handle
	if err := windows.DuplicateHandle(
		process,
		windows.Handle(file.Fd()),
		process,
		&handle,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return nil, err
	}

	owned := os.NewFile(uintptr(handle), file.Name())
	if owned == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}

	r := &contextFileReader{
		ctx:  ctx,
		file: owned,
		done: make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-r.done:
		}
	}()
	return r, nil
}

func (r *contextFileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := r.file.Read(p)
	if n > 0 {
		return n, err
	}
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	return n, err
}

func (r *contextFileReader) Close() error {
	r.closer.Do(func() {
		close(r.done)
		r.closeErr = r.file.Close()
	})
	return r.closeErr
}
