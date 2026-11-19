package loggers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type failingLogWriter struct {
	err error
}

func (w failingLogWriter) Write([]byte) (int, error) { return 0, w.err }

// withTempLogDir returns an isolated directory for an injected file logger.
func withTempLogDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// startFileLogger starts f and returns a stop func that cancels the context and
// joins the logger goroutine.
func startFileLogger(t *testing.T, f *file) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	f.Start(ctx, &wg)
	return func() {
		cancel()
		wg.Wait()
	}
}

func readLogFile(t *testing.T, dir, base string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, base+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading log file: %v", err)
	}
	return string(data)
}

// TestFileLoggerNothingLostOnClose verifies every logged line reaches disk when
// the context is cancelled (clean shutdown): the goroutine drains the buffer
// channel and flushes the 64KB writer before closing the fd.
func TestFileLoggerNothingLostOnClose(t *testing.T) {
	dir := withTempLogDir(t)
	base := "close-test"
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: base}, dir)
	stop := startFileLogger(t, f)

	const n = 500
	var want strings.Builder
	for i := 0; i < n; i++ {
		line := "line-" + strconv.Itoa(i)
		f.Log(time.Now(), line)
		want.WriteString(line + "\n")
	}

	// stop() cancels the context and joins the goroutine, which flushes and
	// closes the fd on the way out — so all output must be on disk afterwards.
	stop()

	if got := readLogFile(t, dir, base); got != want.String() {
		t.Fatalf("lost output on close: got %d bytes, want %d bytes",
			len(got), want.Len())
	}
}

// TestFileLoggerIdleFlush verifies a single low-volume line (follow/tail style)
// is not stuck behind the 64KB buffer: the idle ticker flushes it to disk
// promptly without any explicit Flush or shutdown. The logger goroutine is
// joined via stop() before returning so it cannot outlive the test.
func TestFileLoggerIdleFlush(t *testing.T) {
	dir := withTempLogDir(t)
	base := "idle-test"
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: base}, dir)
	stop := startFileLogger(t, f)
	defer stop()

	f.Log(time.Now(), "follow-line")
	waitForLoggerCondition(t, 2*time.Second, func() bool {
		return strings.Contains(readLogFile(t, dir, base), "follow-line")
	}, func() string {
		return fmt.Sprintf("file sink %q never contained %q; got %q",
			filepath.Join(dir, base+".log"), "follow-line", readLogFile(t, dir, base))
	})
}

// TestFileLoggerExplicitFlush verifies Flush() is SYNCHRONOUS: once it returns,
// the buffered data is already on disk (no polling needed). This is the property
// dlog.FatalPanic relies on to not drop Fatal diagnostics before panicking.
func TestFileLoggerExplicitFlush(t *testing.T) {
	dir := withTempLogDir(t)
	base := "flush-test"
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: base}, dir)
	stop := startFileLogger(t, f)
	defer stop()

	f.Log(time.Now(), "flush-me")
	f.Flush()

	if got := readLogFile(t, dir, base); !strings.Contains(got, "flush-me") {
		t.Fatalf("synchronous Flush() did not persist data before returning; got %q", got)
	}
}

func TestFileLoggerWriteReturnsBufferedWriteError(t *testing.T) {
	wantErr := errors.New("disk full")
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "failure-test"}, "")
	f.lastFileName = "failure-test"
	f.writer = bufio.NewWriterSize(failingLogWriter{err: wantErr}, 1)

	err := f.write(&fileMessageBuf{message: "payload", nl: true})
	if !errors.Is(err, wantErr) {
		t.Fatalf("write error = %v, want %v", err, wantErr)
	}
}

func TestFileLoggerCreateFailuresAreReportedAndMessagesDropped(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T) (string, string)
		wantError string
	}{
		{
			name: "mkdir",
			configure: func(t *testing.T) (string, string) {
				t.Helper()
				tmp := t.TempDir()
				blocker := filepath.Join(tmp, "not-a-directory")
				if err := os.WriteFile(blocker, []byte("block log directory creation"), 0o600); err != nil {
					t.Fatalf("create directory blocker: %v", err)
				}
				return filepath.Join(blocker, "logs"), "failure-test"
			},
			wantError: "create log directory",
		},
		{
			name: "open",
			configure: func(t *testing.T) (string, string) {
				t.Helper()
				return t.TempDir(), filepath.Join("missing", "failure-test")
			},
			wantError: "open log file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logDir, fileBase := tt.configure(t)
			f := newFile(Strategy{Rotation: SignalRotation, FileBase: fileBase}, logDir)
			var stderr bytes.Buffer
			f.errorWriter = &stderr
			stop := startFileLogger(t, f)

			f.Log(time.Now(), "must-be-dropped")
			f.Flush()
			stop()

			if got := stderr.String(); !strings.Contains(got, tt.wantError) {
				t.Fatalf("stderr = %q, want %q", got, tt.wantError)
			}
			if f.fd != nil || f.writer != nil {
				t.Fatal("file logger opened output after output creation failed")
			}
		})
	}
}

func TestFileLoggerFailedRotationPreservesCurrentWriter(t *testing.T) {
	dir := withTempLogDir(t)
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "current"}, dir)

	current, openErr := f.getWriter("current")
	if openErr != nil {
		t.Fatalf("open current writer: %v", openErr)
	}
	if _, writeErr := current.WriteString("before\n"); writeErr != nil {
		t.Fatalf("write before failed rotation: %v", writeErr)
	}

	blocker := filepath.Join(dir, "not-a-directory")
	if writeErr := os.WriteFile(blocker, []byte("block rotated log directory"), 0o600); writeErr != nil {
		t.Fatalf("create rotation blocker: %v", writeErr)
	}
	f.logDir = filepath.Join(blocker, "logs")
	f.lastFileName = "" // The logger goroutine consumed a Rotate signal.
	var stderr bytes.Buffer
	f.errorWriter = &stderr
	rotationErr := f.write(&fileMessageBuf{now: time.Now(), message: "dropped", nl: true})
	f.reportError("write log message", rotationErr)

	if rotationErr == nil || !strings.Contains(rotationErr.Error(), "create log directory") {
		t.Fatalf("rotation error = %v, want create log directory error", rotationErr)
	}
	if got := stderr.String(); !strings.Contains(got, "create log directory") {
		t.Fatalf("stderr = %q, want rotation failure", got)
	}
	if f.writer != current {
		t.Fatal("failed rotation replaced the active writer")
	}
	if _, err := current.WriteString("after\n"); err != nil {
		t.Fatalf("current writer unusable after failed rotation: %v", err)
	}
	if err := current.Flush(); err != nil {
		t.Fatalf("flush current writer: %v", err)
	}
	if err := f.fd.Close(); err != nil {
		t.Fatalf("close current writer: %v", err)
	}
	f.logDir = dir

	if got := readLogFile(t, dir, "current"); got != "before\nafter\n" {
		t.Fatalf("current log contents = %q, want writes before and after failed rotation", got)
	}
}

func TestFileLoggerColorMethodsWritePlainMessages(t *testing.T) {
	dir := withTempLogDir(t)
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "colors"}, dir)
	stop := startFileLogger(t, f)

	now := time.Now()
	f.LogWithColors(now, "plain diagnostic", "\x1b[31mcolored diagnostic\x1b[0m")
	f.RawWithColors(now, "plain payload", "\x1b[31mcolored payload\x1b[0m")
	f.Flush()
	stop()

	if got := readLogFile(t, dir, "colors"); got != "plain diagnostic\nplain payload" {
		t.Fatalf("file contents = %q, want uncolored diagnostic and payload", got)
	}
}

// TestFileLoggerRotateDoesNotBlockWithoutWrites verifies that Rotate() does
// not deadlock when no log messages have been produced. Previously rotateCh
// was unbuffered and only drained opportunistically from write(), so a SIGHUP
// before any Log() call would block the caller forever.
func TestFileLoggerRotateDoesNotBlockWithoutWrites(t *testing.T) {
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "unit-test"}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	f.Start(ctx, &wg)

	done := make(chan struct{})
	go func() {
		f.Rotate()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Rotate() blocked without any writes; expected prompt return")
	}

	cancel()
	wg.Wait()
}

// TestFileLoggerCancelBeforeFirstWriteDoesNotPanic verifies that cancelling
// the context before any write has happened does not panic. Previously the
// goroutine called f.fd.Close() unconditionally, but f.fd is only populated
// by the first getWriter() call, so a ctx cancel with no prior writes
// panicked on a nil pointer.
func TestFileLoggerCancelBeforeFirstWriteDoesNotPanic(t *testing.T) {
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "unit-test"}, "")

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	f.Start(ctx, &wg)

	cancel()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(1 * time.Second):
		t.Fatal("file logger goroutine did not exit after ctx cancel")
	}
}
