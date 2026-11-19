package loggers

import (
	"bufio"
	"context"
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
	// flushPending covers both an unread dirty notification and an armed
	// timer. Protected by mutex, it coalesces a burst into one notification.
	flushPending bool
	dirtyCh      chan struct{}
}

var _ Logger = (*stdout)(nil)
var _ Starter = (*stdout)(nil)
var _ Pauser = (*stdout)(nil)
var _ RawBytesWriter = (*stdout)(nil)

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
	s := &stdout{writer: bufio.NewWriterSize(w, stdoutWriterBufSize), dirtyCh: make(chan struct{}, 1)}
	s.resumeCond = sync.NewCond(&s.mutex)
	return s
}

func (s *stdout) Start(ctx context.Context, wg *sync.WaitGroup) {
	// Arm only on the first buffered write after a flush. Producers never
	// read the clock or reset a timer; an unused logger has no timer wakeups.
	go func() {
		defer wg.Done()
		start := time.Now()
		var timer *time.Timer
		var tick <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			select {
			case <-s.dirtyCh:
				delay := nextFlushDelay(start, stdoutIdleFlushInterval)
				if timer == nil {
					timer = time.NewTimer(delay)
				} else {
					timer.Reset(delay)
				}
				tick = timer.C
			case <-tick:
				tick = nil
				s.flushIdle()
			case <-ctx.Done():
				// Another Start caller may still be alive. Release the
				// pending state along with this worker's final flush so
				// later writes can wake that worker (or a future Start).
				s.flushIdle()
				return
			}
		}
	}()
}

func (s *stdout) Log(message string) {
	s.log(message, true)
}

func (s *stdout) LogWithColors(message, coloredMessage string) {
	s.log(coloredMessage, true)
}

func (s *stdout) Raw(message string) {
	s.log(message, false)
}

func (s *stdout) RawWithColors(message, coloredMessage string) {
	s.log(coloredMessage, false)
}

// RawBytes writes payload bytes verbatim without a string conversion.
func (s *stdout) RawBytes(message []byte) {
	s.lockUnpaused()
	defer s.mutex.Unlock()
	_, _ = s.writer.Write(message)
	s.armIdleFlush()
}

func (s *stdout) log(message string, nl bool) {
	s.lockUnpaused()
	defer s.mutex.Unlock()

	// Buffered writes into the bufio.Writer batch many lines into one write
	// syscall. Errors are intentionally ignored: a logger that cannot write to
	// stdout has nowhere to report the failure.
	_, _ = s.writer.WriteString(message)
	if nl {
		_ = s.writer.WriteByte('\n')
	}
	s.armIdleFlush()
}

// armIdleFlush is called with mutex held. Flush/Pause leave a pending timer
// alone: any later write before it fires is covered by the same deadline.
func (s *stdout) armIdleFlush() {
	if !s.flushPending && s.writer.Buffered() > 0 {
		s.flushPending = true
		signal(s.dirtyCh)
	}
}

func (s *stdout) flushIdle() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_ = s.writer.Flush()
	s.flushPending = false
}

// lockUnpaused acquires the mutex once logging is not paused. The caller must
// unlock it.
func (s *stdout) lockUnpaused() {
	s.mutex.Lock()
	for s.pauseDepth > 0 {
		// Cond.Wait releases the mutex while logging is paused. This lets
		// Resume acquire it and prevents a waiting log call from blocking the
		// interactive path that owns the terminal.
		s.resumeCond.Wait()
	}
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
	_ = s.writer.Flush()
}

func (*stdout) SupportsColors() bool { return true }
