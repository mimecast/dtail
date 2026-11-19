package loggers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	// fileWriterBufSize is the size of the bufio buffer wrapping the log file
	// descriptor. A real buffer (instead of the old 1-byte writer that forced a
	// write syscall per line) lets bulk payload — e.g. dcat/dgrep tee — batch
	// into ~one syscall per bufferful, cutting the client receive-path syscall
	// count and CPU by ~5x. bufio auto-flushes when full, so high-throughput
	// output never stalls in the buffer.
	fileWriterBufSize = 64 * 1024
	// fileIdleFlushInterval bounds how long buffered data may sit unwritten when
	// output goes idle (follow/interactive mode trickling a few lines). Without
	// it, low-volume output would be stuck behind the buffer until it fills or
	// the logger shuts down, so follow/tail would appear frozen on disk.
	fileIdleFlushInterval = 100 * time.Millisecond
	// fileFlushTimeout bounds how long a synchronous Flush() waits for the
	// logger goroutine to acknowledge. It exists purely as a deadlock guard when
	// the goroutine is already gone (e.g. Flush racing shutdown); under normal
	// operation the ack is near-instant.
	fileFlushTimeout = 2 * time.Second
)

type fileMessageBuf struct {
	now     time.Time
	message string
	nl      bool
}

type file struct {
	bufferCh chan *fileMessageBuf
	rotateCh chan struct{}
	// flushCh carries a per-call reply channel so Flush() can block until the
	// logger goroutine has actually drained the buffer channel and flushed the
	// bufio writer to disk. This makes Flush() synchronous, which the crash path
	// (dlog.FatalPanic -> Flush -> panic) relies on: an async signal could let
	// the process unwind before the goroutine drains, dropping up to one buffer
	// (64KB) of Fatal diagnostics.
	flushCh      chan chan struct{}
	fd           *os.File
	writer       *bufio.Writer
	mutex        sync.Mutex
	started      bool
	lastFileName string
	strategy     Strategy
	logDir       string
	errorWriter  io.Writer
}

var _ Logger = (*file)(nil)
var _ Starter = (*file)(nil)
var _ Rotator = (*file)(nil)

func newFile(strategy Strategy, logDir string) *file {
	// Rotate uses a capacity-1, non-blocking coalescing send so callers never
	// block on the logger goroutine (repeated signals collapse into one pending
	// notification). flushCh is unbuffered and carries a reply
	// channel because Flush() is synchronous: it must wait for the goroutine to
	// drain and write before returning.
	return &file{
		bufferCh:    make(chan *fileMessageBuf, runtime.GOMAXPROCS(0)*100),
		rotateCh:    make(chan struct{}, 1),
		flushCh:     make(chan chan struct{}),
		strategy:    strategy,
		logDir:      logDir,
		errorWriter: os.Stderr,
	}
}

func (f *file) Start(ctx context.Context, wg *sync.WaitGroup) {
	f.mutex.Lock()
	defer func() {
		f.started = true
		f.mutex.Unlock()
	}()

	if f.started {
		// Logger already started from another Goroutine.
		wg.Done()
		return
	}

	go func() {
		defer wg.Done()
		// Idle-flush ticker: with a real (64KB) buffer, low-volume output
		// (follow/interactive) would otherwise sit in the buffer until it
		// fills. The ticker flushes any partial buffer promptly so follow/tail
		// output reaches disk within fileIdleFlushInterval. flush() is cheap
		// when nothing is buffered.
		ticker := time.NewTicker(fileIdleFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case m := <-f.bufferCh:
				f.reportError("write log message", f.write(m))
			case <-ticker.C:
				f.reportError("flush idle log output", f.flush())
			case done := <-f.flushCh:
				// Synchronous flush: drain + write, then acknowledge so the
				// blocked Flush() caller can proceed (used by FatalPanic).
				f.reportError("flush requested log output", f.flush())
				close(done)
			case <-f.rotateCh:
				// Force re-opening the outfile on the next write.
				// Drained here (not only from write()) so that Rotate()
				// makes progress even when no log messages arrive.
				f.lastFileName = ""
			case <-ctx.Done():
				f.reportError("flush log output during shutdown", f.flush())
				// f.fd is only populated after the first getWriter() call;
				// guard against a nil pointer when the logger is shut down
				// before anything has been written.
				if f.fd != nil {
					f.reportError("close log file during shutdown", f.fd.Close())
				}
				return
			}
		}
	}()
}

func (f *file) Log(now time.Time, message string) {
	f.bufferCh <- &fileMessageBuf{now, message, true}
}

func (f *file) LogWithColors(now time.Time, message, _ string) {
	f.Log(now, message)
}

func (f *file) Raw(now time.Time, message string) {
	f.bufferCh <- &fileMessageBuf{now, message, false}
}

func (f *file) RawWithColors(now time.Time, message, _ string) {
	f.Raw(now, message)
}

// signal performs a non-blocking, coalescing send on a capacity-1 control
// channel. If a signal is already pending the new one is dropped, which is
// the desired behaviour for idempotent operations such as Rotate.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (f *file) Rotate() { signal(f.rotateCh) }

// Flush synchronously drains any queued messages and writes the bufio buffer to
// disk, blocking until the logger goroutine acknowledges. The crash path
// (dlog.FatalPanic) depends on this: with an async signal the process could
// panic and unwind before the goroutine drained, losing buffered diagnostics.
// A bounded timeout guards against a deadlock when the goroutine has already
// exited (Flush racing shutdown), in which case the ctx.Done path has already
// flushed or will flush.
func (f *file) Flush() {
	done := make(chan struct{})
	select {
	case f.flushCh <- done:
	case <-time.After(fileFlushTimeout):
		return
	}
	select {
	case <-done:
	case <-time.After(fileFlushTimeout):
	}
}

func (*file) SupportsColors() bool { return false }

func (f *file) write(m *fileMessageBuf) error {
	var (
		writer *bufio.Writer
		err    error
	)
	if f.strategy.Rotation == DailyRotation {
		writer, err = f.getWriter(m.now.Format("20060102"))
	} else {
		writer, err = f.getWriter(f.strategy.FileBase)
	}
	if err != nil {
		return err
	}

	if _, err := writer.WriteString(m.message); err != nil {
		return err
	}
	if m.nl {
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

func (f *file) getWriter(name string) (*bufio.Writer, error) {
	if f.lastFileName == name && f.writer != nil {
		return f.writer, nil
	}
	if f.logDir == "" {
		return nil, errors.New("log configuration is unavailable")
	}
	if err := os.MkdirAll(f.logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory %q: %w", f.logDir, err)
	}

	logFile := filepath.Join(f.logDir, name+".log")
	newFd, err := os.OpenFile(logFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", logFile, err)
	}

	// Close old writer.
	if f.fd != nil {
		f.reportError("flush rotated log file", f.writer.Flush())
		f.reportError("close rotated log file", f.fd.Close())
	}
	// Set new writer. Use a real buffer (fileWriterBufSize) so bulk payload
	// batches into few write syscalls instead of one-or-two per line. The
	// logger goroutine's idle ticker and the ctx.Done/flush paths keep
	// low-volume and shutdown output from being stuck in the buffer.
	f.fd = newFd
	f.writer = bufio.NewWriterSize(f.fd, fileWriterBufSize)
	f.lastFileName = name

	return f.writer, nil
}

func (f *file) flush() error {
	var flushErr error
	for {
		select {
		case m := <-f.bufferCh:
			flushErr = errors.Join(flushErr, f.write(m))
		default:
			if f.writer != nil {
				flushErr = errors.Join(flushErr, f.writer.Flush())
			}
			return flushErr
		}
	}
}

func (f *file) reportError(operation string, err error) {
	if err != nil {
		writer := f.errorWriter
		if writer == nil {
			writer = os.Stderr
		}
		_, _ = fmt.Fprintf(writer, "file logger: %s: %v\n", operation, err)
	}
}
