//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package fs

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const contextFilePollInterval = 50 * time.Millisecond

// contextFileReader makes a pollable file read cancellation-aware without
// closing or changing the flags of the caller-owned descriptor. Serverless
// stdin is commonly a pipe whose writer stays open and idle; a direct Read on
// that pipe cannot observe the command context being canceled.
type contextFileReader struct {
	ctx  context.Context
	file *os.File
}

func newContextFileReader(ctx context.Context, file *os.File) (*contextFileReader, error) {
	return &contextFileReader{ctx: ctx, file: file}, nil
}

// Close deliberately leaves the borrowed descriptor open. Unix cancellation
// is implemented with readiness polling, so this reader owns no resource.
func (r *contextFileReader) Close() error { return nil }

func (r *contextFileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	fd := []unix.PollFd{{
		Fd:     int32(r.file.Fd()),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}

		ready, err := unix.Poll(fd, int(contextFilePollInterval.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if ready == 0 {
			continue
		}
		if fd[0].Revents&unix.POLLNVAL != 0 {
			return 0, os.ErrInvalid
		}
		return r.file.Read(p)
	}
}
