package loggers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	// stdoutWriterBufSize is the size of the bufio buffer wrapping os.Stdout.
	// The old path did one fmt.Println (one write syscall) per received line;
	// buffering lets bulk payload batch into ~one syscall per bufferful. bufio
	// auto-flushes when full so high-throughput output never stalls.
	stdoutWriterBufSize = 64 * 1024
	// stdoutIdleFlushInterval bounds how long buffered output may sit unwritten
	// when output goes idle (follow/interactive trickling a few lines). Without
	// it, low-volume output would be stuck behind the buffer, so follow/tail
	// would appear frozen on the terminal.
	stdoutIdleFlushInterval = 100 * time.Millisecond
)

type stdout struct {
	writer     *bufio.Writer
	mutex      sync.Mutex
	resumeCond *sync.Cond
	pauseDepth int
}

var _ Logger = (*stdout)(nil)
var _ Starter = (*stdout)(nil)
var _ Pauser = (*stdout)(nil)

func newStdout() *stdout {
	return newStdoutWriter(os.Stdout)
}

// newStdoutWriter builds a stdout logger over an arbitrary sink. Production
// uses os.Stdout; tests inject a counting writer to assert that buffering
// batches many lines into few underlying writes. The bufio writer is created
// eagerly so the logger is usable even when Start() is never called (e.g. in
// isolated unit tests); idle/shutdown flushing is only driven once Start()
// spawns the flush goroutine.
func newStdoutWriter(w io.Writer) *stdout {
	s := &stdout{writer: bufio.NewWriterSize(w, stdoutWriterBufSize)}
	s.resumeCond = sync.NewCond(&s.mutex)
	return s
}

func (s *stdout) Start(ctx context.Context, wg *sync.WaitGroup) {
	// Background flusher: with a real buffer, low-volume (follow/interactive)
	// output would otherwise sit unwritten until the buffer fills. The ticker
	// flushes any partial buffer promptly, and ctx.Done triggers a final flush
	// so no buffered output is lost on clean shutdown. wg.Done is deferred to
	// the goroutine so callers (ClientRuntime.Stop -> wg.Wait) block until the
	// final flush has happened.
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(stdoutIdleFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.Flush()
			case <-ctx.Done():
				s.Flush()
				return
			}
		}
	}()
}

func (s *stdout) Log(now time.Time, message string) {
	s.log(message, true)
}

func (s *stdout) LogWithColors(now time.Time, message, coloredMessage string) {
	s.log(coloredMessage, true)
}

func (s *stdout) Raw(now time.Time, message string) {
	s.log(message, false)
}

func (s *stdout) RawWithColors(now time.Time, message, coloredMessage string) {
	s.log(coloredMessage, false)
}

func (s *stdout) log(message string, nl bool) {
	s.mutex.Lock()
	for s.pauseDepth > 0 {
		// Cond.Wait releases the mutex while logging is paused. This lets
		// Resume acquire it and prevents a waiting log call from blocking the
		// interactive path that owns the terminal.
		s.resumeCond.Wait()
	}
	defer s.mutex.Unlock()

	// Buffered writes: fmt.Fprint(ln) into the bufio.Writer batches many lines
	// into one write syscall. Errors are intentionally ignored — a logger that
	// cannot write to stdout has nowhere to report the failure.
	if nl {
		_, _ = fmt.Fprintln(s.writer, message)
		return
	}
	_, _ = fmt.Fprint(s.writer, message)
}

func (s *stdout) Pause() {
	// Flush before pausing so all output produced so far is visible before the
	// caller (interactive prompt / stats interrupt) writes directly to stdout,
	// preserving the ordering the unbuffered path used to give for free.
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_ = s.writer.Flush()
	s.pauseDepth++
}

func (s *stdout) Resume() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.pauseDepth == 0 {
		return
	}

	s.pauseDepth--
	if s.pauseDepth == 0 {
		s.resumeCond.Broadcast()
	}
}

func (s *stdout) Flush() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	// bufio.Flush is a no-op when nothing is buffered, so calling this on every
	// idle tick is cheap.
	_ = s.writer.Flush()
}

func (*stdout) SupportsColors() bool { return true }
