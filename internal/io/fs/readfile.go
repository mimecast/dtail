package fs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

const defaultMaxLineLength = 1024 * 1024

var (
	errFileTruncated = errors.New("file got truncated")
	errFileRotated   = errors.New("file got rotated")
)

// ReadOptions configures a file-backed reader.
type ReadOptions struct {
	Mode           omode.Mode
	Target         *ValidatedReadTarget
	FilePath       string
	GlobID         string
	ServerMessages chan<- string
	SeekEOF        bool
	MaxLineLength  int
	Logger         logging.Logger
}

// ReadFile reads and filters a local file or serverless pipe.
type ReadFile struct {
	// Logger for filesystem diagnostics.
	logger logging.Logger
	// Various statistics (e.g. regex hit percentage, transfer percentage).
	stats
	// Path of log file to tail.
	filePath string
	// Rooted target used for validated server-side re-opens.
	validatedTarget *ValidatedReadTarget
	// The glob identifier of the file.
	globID string
	// Channel to send a server message to the dtail client
	serverMessages chan<- string
	// Periodically retry reading file.
	retry bool
	// Can I skip messages when there are too many?
	canSkipLines bool
	// Keep reading when the current file reaches EOF?
	follow bool
	// Seek past existing data on the first successful file open?
	seekInitialEOF bool
	// Warned already about a long line.
	warnedAboutLongLine bool
	// Maximum line length before a line is split.
	maxLineLength int
	// pipeInput overrides os.Stdin for serverless pipe reads. It exists so the
	// cancellation and ownership contract can be tested without replacing the
	// process-wide stdin descriptor.
	pipeInput *os.File
	// truncateCheck is an optional test seam for the periodic child goroutine.
	truncateCheck func(context.Context, chan<- struct{})
	// bufferRecycleObserver is an optional test seam for asserting buffer
	// ownership without relying on sync.Pool retrieval order.
	bufferRecycleObserver func(*bytes.Buffer)
}

var _ FileReader = (*ReadFile)(nil)

// NewReadFile returns a reader configured for a snapshot or follow operation.
func NewReadFile(options ReadOptions) (*ReadFile, error) {
	retry, canSkipLines, follow, err := readBehavior(options.Mode)
	if err != nil {
		return nil, err
	}

	var target *ValidatedReadTarget
	if options.Target != nil {
		if options.Target.Kind != FileKind {
			return nil, fmt.Errorf("read file requires target kind %d, got %d", FileKind, options.Target.Kind)
		}
		targetCopy := *options.Target
		target = &targetCopy
	}

	return &ReadFile{
		logger:          logging.OrNop(options.Logger),
		filePath:        options.FilePath,
		validatedTarget: target,
		globID:          options.GlobID,
		serverMessages:  options.ServerMessages,
		retry:           retry,
		canSkipLines:    canSkipLines,
		follow:          follow,
		seekInitialEOF:  options.SeekEOF,
		maxLineLength:   options.MaxLineLength,
	}, nil
}

func readBehavior(mode omode.Mode) (retry, canSkipLines, follow bool, err error) {
	switch mode {
	case omode.CatClient, omode.GrepClient:
		return false, false, false, nil
	case omode.TailClient:
		return true, true, true, nil
	default:
		return false, false, false, fmt.Errorf("unsupported read mode: %s (%d)", mode, mode)
	}
}

// String returns the string representation of the ReadFile
func (f *ReadFile) String() string {
	return fmt.Sprintf(
		"readFile(filePath:%s,globID:%s,retry:%v,canSkipLines:%v,follow:%v,seekInitialEOF:%v)",
		f.filePath,
		f.globID,
		f.retry,
		f.canSkipLines,
		f.follow,
		f.seekInitialEOF)
}

// FilePath returns the full file path.
func (f *ReadFile) FilePath() string {
	return f.filePath
}

// Retry reading the file on error?
func (f *ReadFile) Retry() bool {
	return f.retry
}

func (f *ReadFile) lineLimit() int {
	if f.maxLineLength <= 0 {
		return defaultMaxLineLength
	}
	return f.maxLineLength
}

func (f *ReadFile) warnAboutLongLine(ctx context.Context) bool {
	if f.warnedAboutLongLine {
		return true
	}

	if f.serverMessages == nil {
		f.warnedAboutLongLine = true
		return true
	}

	select {
	case f.serverMessages <- f.logger.Warn(f.filePath,
		"Long log line, splitting into multiple lines") + "\n":
		f.warnedAboutLongLine = true
		return true
	case <-ctx.Done():
		return false
	}
}

func (f *ReadFile) makeReader(ctx context.Context) (*bufio.Reader, *os.File, io.Closer, error) {
	if f.filePath == "" && f.globID == "-" {
		return f.makePipeReader(ctx)
	}
	return f.makeFileReader()
}

func (f *ReadFile) makeFileReader() (reader *bufio.Reader, fd *os.File, decompressor io.Closer, err error) {
	seekInitialEOF := f.seekInitialEOF
	if fd, err = f.openFile(); err != nil {
		return
	}

	if f.seekInitialEOF {
		if _, err = fd.Seek(0, io.SeekEnd); err != nil {
			return
		}
	}

	reader, decompressor, err = f.makeCompressedFileReader(fd)
	if err == nil {
		f.seekInitialEOF = false
		f.logger.Trace(f.filePath, f.globID, "Opened file reader", "seekInitialEOF", seekInitialEOF)
	}
	return
}

func (f *ReadFile) openFile() (*os.File, error) {
	if f.validatedTarget != nil {
		return f.validatedTarget.Open()
	}
	return os.Open(f.filePath)
}

func (f *ReadFile) makePipeReader(ctx context.Context) (*bufio.Reader, *os.File, io.Closer, error) {
	input := f.pipeInput
	if input == nil {
		input = os.Stdin
	}
	reader, err := newContextFileReader(ctx, input)
	if err != nil {
		return nil, nil, nil, err
	}
	return bufio.NewReader(reader), nil, reader, nil
}

func (f *ReadFile) periodicTruncateCheck(ctx context.Context, truncate chan<- struct{}) {
	ticker := time.NewTicker(time.Second * 3)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			select {
			case truncate <- struct{}{}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (f *ReadFile) startPeriodicTruncateCheck(ctx context.Context, cancel context.CancelFunc,
	truncate chan<- struct{}) <-chan error {
	done := make(chan error, 1)
	check := f.periodicTruncateCheck
	if f.truncateCheck != nil {
		check = f.truncateCheck
	}
	go func() {
		var childErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				childErr = fmt.Errorf("%w: truncate checker: %v", ErrReaderWorkerPanic, recovered)
				logging.OrNop(f.logger).Error(f.filePath, childErr, "stack", string(debug.Stack()))
				cancel()
			}
			done <- childErr
		}()
		check(ctx, truncate)
	}()
	return done
}

func (f *ReadFile) makeCompressedFileReader(fd *os.File) (reader *bufio.Reader, decompressor io.Closer, err error) {
	switch {
	case strings.HasSuffix(f.FilePath(), ".gz"):
		fallthrough
	case strings.HasSuffix(f.FilePath(), ".gzip"):
		f.logger.Info(f.FilePath(), "Detected gzip compression format")
		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(fd)
		if err != nil {
			return
		}
		decompressor = gzipReader
		reader = bufio.NewReader(gzipReader)
	case strings.HasSuffix(f.FilePath(), ".zst"):
		return f.makeZstdReader(fd)
	default:
		reader = bufio.NewReader(fd)
	}
	return
}

// truncated reports whether the open file was truncated in place or replaced
// at its path. A replacement must be detected by identity, because its size can
// equal or exceed the current read offset.
func (f *ReadFile) truncated(fd *os.File) (bool, error) {
	if fd == nil {
		return false, nil
	}

	f.logger.Debug(f.filePath, "File truncation check")

	// Can not seek currently open FD.
	currentPosition, err := fd.Seek(0, io.SeekCurrent)
	if err != nil {
		return true, err
	}
	openInfo, err := fd.Stat()
	if err != nil {
		return true, err
	}

	pathInfo, err := f.pathInfo()
	if err != nil {
		return true, err
	}
	if !os.SameFile(openInfo, pathInfo) {
		return true, errFileRotated
	}
	if currentPosition > pathInfo.Size() {
		return true, errFileTruncated
	}
	return false, nil
}

func (f *ReadFile) pathInfo() (os.FileInfo, error) {
	if f.validatedTarget == nil {
		info, err := os.Stat(f.filePath)
		if err != nil {
			return nil, fmt.Errorf("stat read path %s: %w", f.filePath, err)
		}
		return info, nil
	}

	info, err := os.Lstat(f.validatedTarget.resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("lstat validated read path %s: %w",
			f.validatedTarget.resolvedPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("validated read target is no longer a regular file: %s",
			f.validatedTarget.resolvedPath)
	}
	return info, nil
}
