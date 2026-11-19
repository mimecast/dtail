package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/regex"
)

// The tests in this file check that output too large for the session's
// output buffer is reported to the client (a visible warning and the hidden
// failed-command reason protocol.CommandFailureOutputTooLarge) on every read
// path that can drop it: the shared follow read, the group one-shot read and
// the final flush of a private read. TestServerHandlerReportsOutputTooLargeForTheBuffer
// covers a line dropped by the private read through a whole session.

// tooLargeLineWriter is a session output whose output buffer is too small
// for one write: the first line it gets (failLine) or its first flush
// (failFlush) fails with errOutputPayloadTooLarge, wrapped like the output
// manager wraps it, or with flushErr when that is set. Every other call is
// passed on.
type tooLargeLineWriter struct {
	LineWriter
	failLine  bool
	failFlush bool
	flushErr  error
	failed    *atomic.Bool
}

func (w tooLargeLineWriter) WriteLineData(content []byte, lineNum uint64, sourceID string) error {
	if w.failLine && w.failed.CompareAndSwap(false, true) {
		return tooLargeError()
	}
	return w.LineWriter.WriteLineData(content, lineNum, sourceID)
}

func (w tooLargeLineWriter) Flush() error {
	err := w.LineWriter.Flush()
	if w.failFlush && w.failed.CompareAndSwap(false, true) {
		if w.flushErr != nil {
			return w.flushErr
		}
		return tooLargeError()
	}
	return err
}

func tooLargeError() error {
	return fmt.Errorf("%w: size %d bytes, limit %d bytes", errOutputPayloadTooLarge, 2048, 1024)
}

// newTooLargeReadCommand returns a read command of server whose session
// output is a tooLargeLineWriter configured by writer, and the flag that
// tells whether the writer failed. Its logger returns the text of its
// warnings so that the warning sent to the client shows.
func newTooLargeReadCommand(server *sharedReadTestServer, mode omode.Mode,
	writer tooLargeLineWriter) (*readCommand, *atomic.Bool) {

	dependencies := server.readCommandDependencies()
	dependencies.logger = &framingWarnLogger{}
	writers, failed := dependencies.newLineWriter, &atomic.Bool{}
	dependencies.newLineWriter = func(ctx context.Context, generation uint64) LineWriter {
		w := writer
		w.LineWriter, w.failed = writers(ctx, generation), failed
		return w
	}
	return newReadCommandWithDependencies(dependencies, mode, nil), failed
}

// serverMessages returns the user-visible and hidden messages the session
// sent so far.
func (s *sharedReadTestServer) serverMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.messages)
}

// checkTooLargeReported fails the test unless server got both the warning
// and exactly one failed-command message, with the output reason.
func checkTooLargeReported(t *testing.T, server *sharedReadTestServer) {
	t.Helper()
	want := []string{protocol.HiddenCommandFailedPrefix + protocol.CommandFailureOutputTooLarge}
	waitForShared(t, "the failure report", func() bool {
		return len(server.failureMessages()) > 0
	})
	if got := server.failureMessages(); !slices.Equal(got, want) {
		t.Errorf("failed command messages = %q, want %q", got, want)
	}
	warned := slices.ContainsFunc(server.serverMessages(), func(message string) bool {
		return strings.Contains(message, "Dropped output too large for the session output buffer")
	})
	if !warned {
		t.Errorf("no warning about the dropped output in %q", server.serverMessages())
	}
}

// TestSharedFollowReadReportsOutputTooLarge: a line of a shared follow read
// dropped because it does not fit the session's output buffer fails the
// session's processor; readShared reports it like the private read loop,
// then restarts privately from the file's beginning.
func TestSharedFollowReadReportsOutputTooLarge(t *testing.T) {
	hub := NewReadHub(&config.ServerConfig{ReadRetryIntervalMs: 10}, logging.NopLogger{})
	path := writeSharedReadFile(t, "existing\n")
	server := newSharedReadTestServer(t, hub)
	cmd, failed := newTooLargeReadCommand(server, omode.TailClient, tooLargeLineWriter{failLine: true})
	target, err := server.PrepareReadTarget(path)
	if err != nil {
		t.Fatalf("test setup: no target for %s: %v", path, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd.read(ctx, lcontext.LContext{}, path, &target, "glob", regex.NewNoop())
	}()
	defer func() {
		cancel()
		<-done
	}()
	// The shared read starts at the end of the file, so append until a line
	// reaches the output: the first one is dropped, the private restart then
	// reads the file from its beginning.
	waitForShared(t, "the private restart after the dropped line", func() bool {
		appendSharedReadFile(t, path, "payload\n")
		time.Sleep(20 * time.Millisecond)
		received := server.received()
		return strings.Contains(received, "existing") && strings.Contains(received, "payload")
	})
	if !failed.Load() {
		t.Fatal("test setup: the session's output never dropped a line")
	}
	checkTooLargeReported(t, server)
}

// TestGroupReadReportsOutputTooLarge: a line of a group one-shot read dropped
// because it does not fit the member's output buffer ends the member's read
// with the output reason, not with a failure to read the file.
func TestGroupReadReportsOutputTooLarge(t *testing.T) {
	hub := NewReadHub(&config.ServerConfig{MaxConcurrentCats: 4}, logging.NopLogger{})
	path := writeSharedReadFile(t, "first\nsecond\n")
	server := newSharedReadTestServer(t, hub)
	cmd, failed := newTooLargeReadCommand(server, omode.CatClient, tooLargeLineWriter{failLine: true})
	target, err := server.PrepareReadTarget(path)
	if err != nil {
		t.Fatalf("test setup: no target for %s: %v", path, err)
	}

	// A group of one member starts its read at once.
	cmd.read(contextWithReadShare(context.Background(), "too-large-group:1"), lcontext.LContext{},
		path, &target, "glob", regex.NewNoop())
	if !failed.Load() {
		t.Fatal("test setup: the member's output never dropped a line")
	}
	checkTooLargeReported(t, server)
}

// finishedFileReader is a reader of one iteration that delivers no line and
// never flushes the processor itself, so that only the final flush of
// readViaProcessor writes; it fails with err.
type finishedFileReader struct{ err error }

func (r finishedFileReader) Start(context.Context, lcontext.LContext, line.Processor, regex.Regex) error {
	return r.err
}

func (finishedFileReader) FilePath() string { return "" }
func (finishedFileReader) Retry() bool      { return false }

var _ fs.FileReader = finishedFileReader{}

// TestPrivateReadReportsOutputTooLargeOnTheFinalFlush: when the final flush
// of a read iteration drops the lines still batched in the writer because
// they do not fit the session's output buffer, readViaProcessor fails the
// iteration (errors.Join of the reader's and the flush error) and the read
// loop reports it; any other flush error stays logged only.
func TestPrivateReadReportsOutputTooLargeOnTheFinalFlush(t *testing.T) {
	readErr := errors.New("reader failed")
	tests := []struct {
		name       string
		readErr    error
		flushErr   error
		wantErrs   []error
		wantReport string
	}{
		{name: "output too large", wantErrs: []error{errOutputPayloadTooLarge},
			wantReport: protocol.CommandFailureOutputTooLarge},
		{name: "output too large after a failed read", readErr: readErr,
			wantErrs:   []error{readErr, errOutputPayloadTooLarge},
			wantReport: protocol.CommandFailureOutputTooLarge},
		{name: "other flush error", flushErr: errors.New("session ended")},
		{name: "other flush error after a failed read", readErr: readErr,
			flushErr: errors.New("session ended"), wantErrs: []error{readErr},
			wantReport: readFailureReadingFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newSharedReadTestServer(t, nil)
			cmd, failed := newTooLargeReadCommand(server, omode.CatClient,
				tooLargeLineWriter{failFlush: true, flushErr: tt.flushErr})
			reader := finishedFileReader{err: tt.readErr}
			ctx := context.Background()
			strategy := cmd.readViaProcessor("path", "glob", cmd.newLineWriter(ctx, cmd.generation))

			err := strategy(ctx, lcontext.LContext{}, reader, regex.NewNoop())
			if !failed.Load() {
				t.Fatal("test setup: the final flush never failed")
			}
			checkIterationError(t, err, tt.wantErrs)

			// The read loop reports what the iteration returned.
			cmd.executeReadLoop(ctx, lcontext.LContext{}, "path", "glob", regex.NewNoop(), reader,
				func(context.Context, lcontext.LContext, fs.FileReader, regex.Regex) error { return err })
			checkReported(t, server, tt.wantReport)
		})
	}
}

// checkIterationError fails the test unless err wraps every error of want,
// or is nil when want is empty.
func checkIterationError(t *testing.T, err error, want []error) {
	t.Helper()
	for _, w := range want {
		if !errors.Is(err, w) {
			t.Errorf("iteration error %v is no %v", err, w)
		}
	}
	if len(want) == 0 && err != nil {
		t.Errorf("iteration error = %v, want none", err)
	}
}

// checkReported fails the test unless server got the failed-command reason
// reason (with the warning for output too large), or none when reason is
// empty.
func checkReported(t *testing.T, server *sharedReadTestServer, reason string) {
	t.Helper()
	if reason == protocol.CommandFailureOutputTooLarge {
		checkTooLargeReported(t, server)
		return
	}
	var want []string
	if reason != "" {
		want = []string{protocol.HiddenCommandFailedPrefix + reason}
	}
	waitForShared(t, "the session's messages", func() bool { return len(server.serverMessage) == 0 })
	drained(t, server)
	if got := server.failureMessages(); !slices.Equal(got, want) {
		t.Errorf("failed command messages = %q, want %q", got, want)
	}
}

// TestHubFailureReportsOutputTooLargeAfterAnEviction: after an eviction the
// hub reads the session's file with a private reader of its own and reports
// each failed iteration through readhub.Session.ReportFailure, with the error
// (stubbed here as the hub would pass it). Output dropped as too large for the
// session's output buffer is reported with the output reason and its warning,
// like the read command's own private read loop does; a failed read of the
// file, or a failure the hub has no error for, stays a failed read.
func TestHubFailureReportsOutputTooLargeAfterAnEviction(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReport string
	}{
		{name: "output too large", err: fmt.Errorf("process line: %w", tooLargeError()),
			wantReport: protocol.CommandFailureOutputTooLarge},
		{name: "failed read", err: errors.New("read failed"), wantReport: readFailureReadingFile},
		{name: "no error", wantReport: readFailureReadingFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newSharedReadTestServer(t, nil)
			cmd, _ := newTooLargeReadCommand(server, omode.TailClient, tooLargeLineWriter{})

			cmd.reportHubFailure(context.Background())(tt.err)
			checkReported(t, server, tt.wantReport)
		})
	}
}
