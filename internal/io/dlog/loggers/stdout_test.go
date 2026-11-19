package loggers

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingWriter records how many times Write is called and accumulates all
// bytes, so tests can assert that buffering batches many logical lines into a
// small number of underlying writes while preserving content and order.
type countingWriter struct {
	mutex  sync.Mutex
	buf    bytes.Buffer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.writes++
	return c.buf.Write(p)
}

func (c *countingWriter) String() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.buf.String()
}

func (c *countingWriter) Writes() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.writes
}

// TestStdoutBuffersAndPreservesOrder proves the buffered stdout path batches
// many lines into far fewer underlying writes than one-per-line, and that a
// Flush emits the exact content in order (nothing dropped or reordered).
func TestStdoutBuffersAndPreservesOrder(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)

	const n = 1000
	var want strings.Builder
	for i := 0; i < n; i++ {
		line := "line-" + strconv.Itoa(i)
		s.Raw(line + "\n")
		want.WriteString(line + "\n")
	}

	// Before flush the small lines must still be batched in the bufio buffer:
	// with per-line writes this would already be n writes.
	if got := cw.Writes(); got >= n {
		t.Fatalf("expected buffering to batch writes, got %d writes for %d lines", got, n)
	}

	s.Flush()

	if got := cw.String(); got != want.String() {
		t.Fatalf("content mismatch after flush:\n got %q\nwant %q", got, want.String())
	}
	// 1000 short lines fit in a handful of 64KB flushes, definitely far below n.
	if got := cw.Writes(); got > n/10 {
		t.Fatalf("expected far fewer than %d writes, got %d", n/10, got)
	}
}

// TestStdoutPauseReturnsAndFlushesWithoutLogCall proves Pause does not need a
// concurrent logger call to complete and flushes earlier output before it
// returns. The quiet unknown-host prompt depends on both properties.
func TestStdoutPauseReturnsAndFlushesWithoutLogCall(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)

	s.Raw("before-pause\n")

	paused := make(chan struct{})
	go func() {
		s.Pause()
		close(paused)
	}()
	waitForSignal(t, paused, "Pause blocked without a concurrent log call")

	// Pause flushes before returning, so the pre-pause line must already
	// be in the sink now regardless of the buffer.
	if got := cw.String(); !strings.Contains(got, "before-pause") {
		t.Fatalf("expected buffered output flushed on Pause, got %q", got)
	}
	s.Resume()
}

// TestStdoutBlocksEveryLogPathWhilePaused verifies that every public logging
// entry point waits until Resume and that no paused output reaches the sink.
func TestStdoutBlocksEveryLogPathWhilePaused(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)
	s.Pause()

	writes := []struct {
		name  string
		write func()
	}{
		{name: "Log", write: func() { s.Log("[log]") }},
		{name: "LogWithColors", write: func() { s.LogWithColors("[plain-log]", "[colored-log]") }},
		{name: "Raw", write: func() { s.Raw("[raw]") }},
		{name: "RawWithColors", write: func() { s.RawWithColors("[plain-raw]", "[colored-raw]") }},
		{name: "RawBytes", write: func() { s.RawBytes([]byte("[raw-bytes]")) }},
	}

	done := make([]chan struct{}, len(writes))
	for i, test := range writes {
		done[i] = make(chan struct{})
		go func() {
			test.write()
			close(done[i])
		}()
	}

	for i, ch := range done {
		assertNoSignal(t, ch, writes[i].name+" returned while stdout was paused")
	}
	if got := cw.String(); got != "" {
		t.Fatalf("paused output reached the sink: %q", got)
	}

	s.Resume()
	for i, ch := range done {
		waitForSignal(t, ch, writes[i].name+" did not resume")
	}
	s.Flush()

	got := cw.String()
	for _, message := range []string{"[log]\n", "[colored-log]\n", "[raw]", "[colored-raw]", "[raw-bytes]"} {
		if count := strings.Count(got, message); count != 1 {
			t.Errorf("resumed output contains %d copies of %q in %q, want 1", count, message, got)
		}
	}
	for _, message := range []string{"[plain-log]", "[plain-raw]"} {
		if strings.Contains(got, message) {
			t.Errorf("colored logging wrote unused plain message %q in %q", message, got)
		}
	}
}

// TestStdoutPausePreservesPromptOrdering verifies that output before the pause,
// direct terminal output during it, and resumed logging cannot be reordered.
func TestStdoutPausePreservesPromptOrdering(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)
	s.Raw("before\n")
	s.Pause()

	logged := make(chan struct{})
	go func() {
		s.Raw("after\n")
		close(logged)
	}()
	assertNoSignal(t, logged, "log returned while direct prompt output owned the sink")

	if _, err := cw.Write([]byte("prompt\n")); err != nil {
		t.Fatalf("write prompt output: %v", err)
	}
	s.Resume()
	waitForSignal(t, logged, "log did not resume after prompt output")
	s.Flush()

	if got, want := cw.String(), "before\nprompt\nafter\n"; got != want {
		t.Fatalf("output order mismatch: got %q, want %q", got, want)
	}
}

// TestStdoutNestedPauseRequiresMatchingResumes verifies overlapping terminal
// owners cannot accidentally resume logging until every pause has ended.
func TestStdoutNestedPauseRequiresMatchingResumes(t *testing.T) {
	s := newStdoutWriter(&countingWriter{})
	s.Pause()
	s.Pause()

	logged := make(chan struct{})
	go func() {
		s.Log("blocked")
		close(logged)
	}()
	assertNoSignal(t, logged, "log returned during nested pause")

	s.Resume()
	assertNoSignal(t, logged, "one Resume ended two nested pauses")
	s.Resume()
	waitForSignal(t, logged, "matching Resumes did not unblock logging")

	// An unmatched Resume is harmless and must never wait for a logger call.
	resumed := make(chan struct{})
	go func() {
		s.Resume()
		close(resumed)
	}()
	waitForSignal(t, resumed, "Resume blocked while stdout was not paused")
}

// TestStdoutIdleFlush proves that a single low-volume line (follow/tail style)
// is not stuck behind the buffer: the Start() idle ticker flushes it promptly
// without any explicit Flush call.
func TestStdoutIdleFlush(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	s.Start(ctx, &wg)

	s.Raw("follow-line\n")
	waitForLoggerCondition(t, 2*time.Second, func() bool {
		return strings.Contains(cw.String(), "follow-line")
	}, func() string {
		return fmt.Sprintf("stdout sink never contained %q; got %q", "follow-line", cw.String())
	})
	cancel()
	wg.Wait()
}

func waitForLoggerCondition(t *testing.T, timeout time.Duration, condition func() bool, diagnostic func() string) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal(diagnostic())
		}
	}
}

// TestStdoutFinalFlushOnClose proves no buffered output is lost on clean
// shutdown: data logged just before ctx cancel is flushed before the Start
// goroutine (and thus wg.Wait) returns.
func TestStdoutFinalFlushOnClose(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	s.Start(ctx, &wg)

	s.Raw("last-line-before-exit\n")
	cancel()
	wg.Wait()

	if got := cw.String(); !strings.Contains(got, "last-line-before-exit") {
		t.Fatalf("buffered output lost on shutdown, got %q", got)
	}
}

// TestStdoutShutdownWhilePaused verifies cancellation and final flushing do not
// depend on Resume, while a paused log remains blocked until explicitly resumed.
func TestStdoutShutdownWhilePaused(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)
	ctx, cancel := context.WithCancel(context.Background())
	var flushers sync.WaitGroup
	flushers.Add(1)
	s.Start(ctx, &flushers)

	s.Raw("before-shutdown\n")
	s.Pause()
	logged := make(chan struct{})
	go func() {
		s.Raw("after-resume\n")
		close(logged)
	}()
	assertNoSignal(t, logged, "log returned while stdout was paused")

	cancel()
	shutdown := make(chan struct{})
	go func() {
		flushers.Wait()
		close(shutdown)
	}()
	waitForSignal(t, shutdown, "stdout flusher did not stop while logging was paused")
	assertNoSignal(t, logged, "shutdown bypassed the active pause")

	s.Resume()
	waitForSignal(t, logged, "paused log did not return after shutdown and Resume")
	s.Flush()
	if got, want := cw.String(), "before-shutdown\nafter-resume\n"; got != want {
		t.Fatalf("shutdown output mismatch: got %q, want %q", got, want)
	}
}

func TestStdoutConcurrentPauseResume(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)
	start := make(chan struct{})

	const (
		writers       = 8
		writesPerLoop = 200
		pauseCycles   = 200
	)
	var workers sync.WaitGroup
	workers.Add(writers + 1)
	for worker := 0; worker < writers; worker++ {
		go func() {
			defer workers.Done()
			<-start
			for i := 0; i < writesPerLoop; i++ {
				s.Raw("line\n")
			}
		}()
	}
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < pauseCycles; i++ {
			s.Pause()
			runtime.Gosched()
			s.Resume()
		}
	}()

	close(start)
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	waitForSignal(t, done, "concurrent writers and pause cycles did not finish")
	s.Flush()

	if got, want := strings.Count(cw.String(), "line\n"), writers*writesPerLoop; got != want {
		t.Fatalf("logged line count: got %d, want %d", got, want)
	}
}

func waitForSignal(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func assertNoSignal(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(failure)
	case <-time.After(25 * time.Millisecond):
	}
}

// TestStdoutRawBytesMatchesRaw checks that the byte-slice payload path writes
// exactly what Raw writes, including empty messages and messages larger than
// the bufio buffer, and that the sink does not keep the caller's slice.
func TestStdoutRawBytesMatchesRaw(t *testing.T) {
	messages := []string{"", "line\n", "no newline", "\n", strings.Repeat("x", 3*stdoutWriterBufSize) + "\n", "tail"}

	viaRaw := &countingWriter{}
	rawSink := newStdoutWriter(viaRaw)
	viaBytes := &countingWriter{}
	bytesSink := newStdoutWriter(viaBytes)
	for _, message := range messages {
		rawSink.Raw(message)
		buf := []byte(message)
		bytesSink.RawBytes(buf)
		for i := range buf {
			buf[i] = '!' // the caller reuses its buffer after the call returns
		}
	}
	rawSink.Flush()
	bytesSink.Flush()

	if viaBytes.String() != viaRaw.String() {
		t.Fatalf("RawBytes output differs from Raw: got %d bytes, want %d bytes",
			len(viaBytes.String()), len(viaRaw.String()))
	}
}

// TestStdoutLogAlwaysAppendsNewline pins the diagnostic newline behavior after
// fmt.Fprintln was replaced by direct buffer writes: Log appends a newline even
// when the message already ends with one, and Raw appends none.
func TestStdoutLogAlwaysAppendsNewline(t *testing.T) {
	cw := &countingWriter{}
	s := newStdoutWriter(cw)
	s.Log("diag")
	s.Log("")
	s.Log("already\n")
	s.Raw("raw")
	s.Flush()

	if got, want := cw.String(), "diag\n\nalready\n\nraw"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
