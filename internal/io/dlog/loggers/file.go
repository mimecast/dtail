package loggers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// dailyFileNameLayout is the time layout of the daily log file base name.
	dailyFileNameLayout = "20060102"
)

type file struct {
	queue    *fileQueue
	rotateCh chan struct{}
	// flushCh carries a per-call reply channel so Flush() can block until the
	// logger goroutine has actually drained the batch queue and flushed the
	// bufio writer to disk. This makes Flush() synchronous, which the crash path
	// (dlog.FatalPanic -> Flush -> panic) relies on: an async signal could let
	// the process unwind before the goroutine drains, dropping up to one buffer
	// (64KB) of Fatal diagnostics.
	flushCh      chan chan struct{}
	fd           *os.File
	writer       *bufio.Writer
	continuation *bufio.Writer // pin the destination through a multi-chunk call
	failedCall   bool          // discard the rest of a call after its first I/O failure
	mutex        sync.Mutex
	started      bool
	lastFileName string
	strategy     Strategy
	logDir       string
	errorWriter  io.Writer
	errorMu      sync.Mutex // shutdown-rejected producers can report concurrently
	wrote        bool       // logger goroutine: a write occurred since the last flush tick
	// clock is the wall-time source for the daily file name. Production uses
	// time.Now; tests inject a fake clock to exercise day rotation.
	clock func() time.Time
	// day caches the daily file base name so write() does not read the clock
	// per message (a clock read costs ~8 µs on hosts whose clocksource the
	// vDSO cannot read, such as hpet, and used to dominate client CPU on bulk
	// payload). It is owned by the logger
	// goroutine: filled on the first daily write after idle and refreshed on
	// active flush ticks. Midnight rotation typically follows within one or two
	// idle-flush intervals, but that is not a bound: select picks randomly
	// among ready cases and a slow sink delays timer handling. The target file
	// is decided at batch write time, not at log time, so queued messages go
	// to the day cached when they are written. A call spanning chunks keeps
	// one destination until its end, and a diagnostic stamped just after midnight can land
	// in the previous day's file (or vice versa). This is an accepted
	// trade-off for not reading the clock per message.
	day string
}

var _ Logger = (*file)(nil)
var _ Starter = (*file)(nil)
var _ Rotator = (*file)(nil)
var _ RawBytesWriter = (*file)(nil)

func newFile(strategy Strategy, logDir string) *file {
	// Rotate uses a capacity-1, non-blocking coalescing send so callers never
	// block on the logger goroutine (repeated signals collapse into one pending
	// notification). flushCh is unbuffered and carries a reply
	// channel because Flush() is synchronous: it must wait for the goroutine to
	// drain and write before returning.
	return &file{
		queue:       newFileQueue(fileQueueChunks),
		rotateCh:    make(chan struct{}, 1),
		flushCh:     make(chan chan struct{}),
		strategy:    strategy,
		logDir:      logDir,
		errorWriter: os.Stderr,
		clock:       time.Now,
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
		f.run(ctx)
	}()
}

func (f *file) Log(message string) {
	f.enqueue(message, nil, true)
}

func (f *file) LogWithColors(message, _ string) {
	f.Log(message)
}

func (f *file) Raw(message string) {
	f.enqueue(message, nil, false)
}

// RawBytes copies borrowed payload into the same owned batches as Raw and Log.
func (f *file) RawBytes(message []byte) {
	f.enqueue("", message, false)
}

func (f *file) RawWithColors(message, _ string) {
	f.Raw(message)
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

func (f *file) run(ctx context.Context) {
	start := time.Now()
	var timer *time.Timer
	var tick <-chan time.Time
	defer f.shutdown()
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-f.queue.ready:
			// Even a partial chunk must arm a flush. Full chunks move
			// immediately; partial chunks coalesce until the next tick.
			f.wrote = true
			if data := f.queue.take(false); data.data != nil {
				f.reportError("write log message", f.writeBatch(data))
			}
		case <-tick:
			tick = nil
			// Bound timer work so continuous producers cannot prevent day
			// refresh. Explicit/shutdown Flush still drains everything.
			f.reportError("flush idle log output", f.drain(fileQueueChunks+1))
			if f.wrote {
				// Refresh even if bulk writes auto-flushed the buffer. Only
				// subsequent writes select the new day; buffered bytes stay put.
				f.refreshDay()
				f.reportError("flush idle log output", f.flushWriter())
				f.wrote = false
				// Keep one more tick to detect a quiet interval. Timer work
				// stays in this worker, not in the per-message write path.
				timer.Reset(nextFlushDelay(start, fileIdleFlushInterval))
				tick = timer.C
			} else {
				f.day = "" // First write after idle must refresh the cached day.
			}
		case done := <-f.flushCh:
			f.reportError("flush requested log output", f.flush())
			close(done)
		case <-f.rotateCh:
			f.lastFileName = ""
		case <-ctx.Done():
			return
		}
		if f.wrote && tick == nil {
			delay := nextFlushDelay(start, fileIdleFlushInterval)
			if timer == nil {
				timer = time.NewTimer(delay)
			} else {
				timer.Reset(delay)
			}
			tick = timer.C
		}
	}
}

func (f *file) shutdown() {
	f.queue.close()
	for {
		f.reportError("flush log output during shutdown", f.flush())
		if f.queue.drained() {
			break
		}
	}
	if f.fd != nil {
		f.reportError("close log file during shutdown", f.fd.Close())
	}
}

// refreshDay re-reads the clock and caches the daily file base name. Strategies
// other than daily rotation never read the clock.
func (f *file) refreshDay() {
	if f.strategy.Rotation != DailyRotation {
		return
	}
	f.day = f.clock().Format(dailyFileNameLayout)
}

// fileName returns the base name of the file the next message goes to. For
// daily rotation it serves the cached day, reading the clock only when the
// cache is empty (first write, or first write after the timer went idle).
func (f *file) fileName() string {
	if f.strategy.Rotation != DailyRotation {
		return f.strategy.FileBase
	}
	if f.day == "" {
		f.refreshDay()
	}
	return f.day
}

func (f *file) enqueue(message string, raw []byte, newline bool) {
	if !f.queue.append(message, raw, newline) {
		f.reportError("write log message", errors.New("logger is shut down"))
	}
}

func (f *file) write(data []byte) error {
	f.wrote = true
	writer := f.continuation
	if writer == nil {
		var err error
		writer, err = f.getWriter(f.fileName())
		if err != nil {
			return err
		}
	}

	if _, err := writer.Write(data); err != nil {
		return err
	}
	if writer.Buffered() == fileWriterBufSize {
		return writer.Flush()
	}
	return nil
}

func (f *file) writeBatch(chunk fileChunk) error {
	f.wrote = true
	var err error
	if !f.failedCall {
		if f.continuation == nil {
			// Pin the selected writer before I/O, including partial failures.
			// Failed selection must not reuse the preceding call's writer.
			f.continuation, err = f.getWriter(f.fileName())
		}
		if err == nil {
			err = f.write(chunk.data)
		}
		// As with a single Write, failure abandons the unwritten suffix.
		// Retrying after rotation could otherwise emit an orphaned suffix.
		f.failedCall = err != nil
	}
	if chunk.end {
		f.continuation = nil
		f.failedCall = false
	}
	f.queue.release(chunk.data)
	return err
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
	// logger goroutine's active timer and the ctx.Done/flush paths keep
	// low-volume and shutdown output from being stuck in the buffer.
	f.fd = newFd
	f.writer = bufio.NewWriterSize(f.fd, fileWriterBufSize)
	f.lastFileName = name

	return f.writer, nil
}

func (f *file) flush() error {
	return errors.Join(f.drain(-1), f.flushWriter())
}

// drain processes at most limit chunks; a negative limit drains until empty.
func (f *file) drain(limit int) error {
	var err error
	for i := 0; limit < 0 || i < limit; i++ {
		data := f.queue.take(true)
		if data.data == nil {
			break
		}
		err = errors.Join(err, f.writeBatch(data))
	}
	return err
}

func (f *file) flushWriter() error {
	if f.writer != nil {
		return f.writer.Flush()
	}
	return nil
}

func (f *file) reportError(operation string, err error) {
	if err != nil {
		f.errorMu.Lock()
		defer f.errorMu.Unlock()
		writer := f.errorWriter
		if writer == nil {
			writer = os.Stderr
		}
		_, _ = fmt.Fprintf(writer, "file logger: %s: %v\n", operation, err)
	}
}
