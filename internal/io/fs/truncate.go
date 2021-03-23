package fs

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/io/logger"
)

func (f readFile) truncateTimer(ctx context.Context) (checkTruncate chan struct{}) {
	checkTruncate = make(chan struct{})

	go func() {
		for {
			select {
			case <-time.After(time.Second * 3):
				select {
				case checkTruncate <- struct{}{}:
				case <-ctx.Done():
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return
}

// Check wether log file is truncated. Returns nil if not.
func (f readFile) truncated(fd *os.File) (bool, error) {
	logger.Debug(f.filePath, "File truncation check")

	// Can not seek currently open FD.
	curPos, err := fd.Seek(0, os.SEEK_CUR)
	if err != nil {
		return true, err
	}

	// Can not open file at original path.
	pathFd, err := os.Open(f.filePath)
	if err != nil {
		return true, err
	}
	defer pathFd.Close()

	// Can not seek file at original path.
	pathPos, err := pathFd.Seek(0, io.SeekEnd)
	if err != nil {
		return true, err
	}

	if curPos > pathPos {
		return true, errors.New("File got truncated")
	}

	return false, nil
}
