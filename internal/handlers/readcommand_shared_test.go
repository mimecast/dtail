package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/regex"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

const sharedReadTimeout = 10 * time.Second

// sharedReadTestServer is a read command server for file reads that records
// the output it receives.
type sharedReadTestServer struct {
	tailLimiter   chan struct{}
	catLimiter    chan struct{}
	outputLines   chan []byte
	serverMessage chan string
	denied        map[string]bool
	readHub       *readhub.Hub

	mu       sync.Mutex
	output   bytes.Buffer
	messages []string
	pending  int32
}

func newSharedReadTestServer(t *testing.T, hub *readhub.Hub) *sharedReadTestServer {
	t.Helper()
	s := &sharedReadTestServer{
		tailLimiter:   make(chan struct{}, 8),
		catLimiter:    make(chan struct{}, 8),
		outputLines:   make(chan []byte, 64),
		serverMessage: make(chan string, 64),
		denied:        map[string]bool{},
		readHub:       hub,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case data := <-s.outputLines:
				s.mu.Lock()
				s.output.Write(data)
				s.mu.Unlock()
			case message := <-s.serverMessage:
				s.mu.Lock()
				s.messages = append(s.messages, message)
				s.mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s
}

func (s *sharedReadTestServer) received() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.String()
}

// failureMessages returns the hidden failed-command messages the session sent.
func (s *sharedReadTestServer) failureMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures []string
	for _, message := range s.messages {
		if strings.HasPrefix(message, protocol.HiddenCommandFailedPrefix) {
			failures = append(failures, message)
		}
	}
	return failures
}

func (s *sharedReadTestServer) PrepareReadTarget(path string) (fs.ValidatedReadTarget, error) {
	if s.denied[path] {
		return fs.ValidatedReadTarget{}, user.ErrReadPermissionDenied
	}
	return fs.NewValidatedReadTarget(path)
}

func (s *sharedReadTestServer) readCommandDependencies() readCommandDependencies {
	return readCommandDependencies{
		server:       s,
		lifecycle:    s,
		aggregates:   s,
		logger:       logging.NopLogger{},
		readerLogger: logging.NopLogger{},
		logContext:   "shared-read-test",
		timings: readTimings{
			globRetryInterval:         time.Millisecond,
			readRetryInterval:         10 * time.Millisecond,
			outputEOFAckTimeout:       time.Millisecond,
			legacyAggregateInputGrace: time.Millisecond,
			maxLineLength:             1024 * 1024,
			maxGlobTargets:            1000,
		},
		newLineWriter: func(ctx context.Context, generation uint64) LineWriter {
			return NewNetworkWriter(ctx, s.outputLines, s.serverMessage, "testhost", true, false,
				generation, func() uint64 { return 0 }, logging.NopLogger{})
		},
		readHub: s.readHub,
	}
}

func (s *sharedReadTestServer) AcquireReadSlot(ctx context.Context, mode omode.Mode, _ string) (func(), bool) {
	limiter := s.tailLimiter
	if mode != omode.TailClient {
		limiter = s.catLimiter
	}
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (s *sharedReadTestServer) TryAcquireReadSlot(mode omode.Mode, _ string) (func(), bool) {
	limiter := s.tailLimiter
	if mode != omode.TailClient {
		limiter = s.catLimiter
	}
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, true
	default:
		return nil, false
	}
}

func (s *sharedReadTestServer) SendReadMessage(ctx context.Context, _ uint64, message string) {
	select {
	case s.serverMessage <- message:
	case <-ctx.Done():
	}
}

func (s *sharedReadTestServer) NewReadMessages(ctx context.Context, _ uint64) (chan string, func()) {
	messages := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case _, ok := <-messages:
				if !ok {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return messages, func() {
		close(messages)
		<-done
	}
}

func (*sharedReadTestServer) FinishReadBatch(context.Context, omode.Mode, uint64) {}
func (*sharedReadTestServer) DebugReadLifecycle(string, ...any)                   {}
func (*sharedReadTestServer) Aggregate() *mapaggregate.Aggregate                  { return nil }
func (*sharedReadTestServer) TriggerShutdown(context.Context)                     {}

func (s *sharedReadTestServer) AddPendingFiles(delta int32) int32 {
	return atomic.AddInt32(&s.pending, delta)
}

func (s *sharedReadTestServer) CompletePendingFile() (int32, int32) {
	return atomic.AddInt32(&s.pending, -1), 0
}

func (s *sharedReadTestServer) PendingAndActive() (int32, int32) {
	return atomic.LoadInt32(&s.pending), 0
}

func writeSharedReadFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendSharedReadFile(t *testing.T, path, text string) {
	t.Helper()
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()
	if _, err := fd.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func waitForShared(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(sharedReadTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startTailRead runs a tail read of path on its own read command.
func startTailRead(t *testing.T, server *sharedReadTestServer, path string) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	cmd := newReadCommandWithDependencies(server.readCommandDependencies(), omode.TailClient, nil)
	target, err := server.PrepareReadTarget(path)
	if err != nil {
		t.Fatalf("test setup: no target for %s", path)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd.read(ctx, lcontext.LContext{}, path, &target, "glob", regex.NewNoop())
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel, done
}

func TestShouldShareRead(t *testing.T) {
	file := writeSharedReadFile(t, "x\n")
	fileTarget, err := fs.NewValidatedReadTarget(file)
	if err != nil {
		t.Fatal(err)
	}
	journalTarget, err := fs.NewValidatedJournalTarget("journal:dtail.service")
	if err != nil {
		t.Fatal(err)
	}
	follow := func(context.Context, readhub.Session) error { return nil }

	tests := []struct {
		name   string
		mutate func(*readCommand, *lcontext.LContext, **fs.ValidatedReadTarget)
		path   string
		want   bool
	}{
		{"tail of a file with a hub", func(*readCommand, *lcontext.LContext, **fs.ValidatedReadTarget) {}, "", true},
		{"local context", func(_ *readCommand, ltx *lcontext.LContext, _ **fs.ValidatedReadTarget) {
			ltx.BeforeContext, ltx.AfterContext = 2, 1
		}, "", true},
		{"no hub", func(r *readCommand, _ *lcontext.LContext, _ **fs.ValidatedReadTarget) { r.followShared = nil }, "", false},
		{"serverless", func(r *readCommand, _ *lcontext.LContext, _ **fs.ValidatedReadTarget) { r.serverless = true }, "", false},
		{"cat", func(r *readCommand, _ *lcontext.LContext, _ **fs.ValidatedReadTarget) { r.mode = omode.CatClient }, "", false},
		{"grep", func(r *readCommand, _ *lcontext.LContext, _ **fs.ValidatedReadTarget) { r.mode = omode.GrepClient }, "", false},
		{"journal", func(_ *readCommand, _ *lcontext.LContext, target **fs.ValidatedReadTarget) {
			*target = &journalTarget
		}, "", false},
		{"no target", func(_ *readCommand, _ *lcontext.LContext, target **fs.ValidatedReadTarget) { *target = nil }, "", false},
		{"max count", func(_ *readCommand, ltx *lcontext.LContext, _ **fs.ValidatedReadTarget) { ltx.MaxCount = 3 }, "", false},
		{"gzip", func(*readCommand, *lcontext.LContext, **fs.ValidatedReadTarget) {}, "app.log.gz", false},
		{"zstd", func(*readCommand, *lcontext.LContext, **fs.ValidatedReadTarget) {}, "app.log.zst", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &readCommand{mode: omode.TailClient, followShared: follow}
			ltx := lcontext.LContext{}
			target := &fileTarget
			tt.mutate(r, &ltx, &target)
			path := tt.path
			if path == "" {
				path = file
			}
			if got := r.shouldShareRead(ltx, target, path); got != tt.want {
				t.Errorf("shouldShareRead() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewReadHubHonoursSharedReadsDisable(t *testing.T) {
	if NewReadHub(nil, nil) != nil {
		t.Error("NewReadHub(nil) made a hub")
	}
	// Shared reads are on by default in dserver.
	if NewReadHub(&config.ServerConfig{}, nil) == nil {
		t.Error("NewReadHub made no hub with the default configuration")
	}
	if NewReadHub(&config.ServerConfig{SharedReadsDisable: true}, nil) != nil {
		t.Error("NewReadHub made a hub although shared reads are disabled")
	}
}

func TestSharedTailReadsShareOneReader(t *testing.T) {
	hub := NewReadHub(&config.ServerConfig{ReadRetryIntervalMs: 10}, logging.NopLogger{})
	path := writeSharedReadFile(t, "existing\n")
	first := newSharedReadTestServer(t, hub)
	second := newSharedReadTestServer(t, hub)
	cancelFirst, firstDone := startTailRead(t, first, path)
	cancelSecond, secondDone := startTailRead(t, second, path)
	waitForShared(t, "both sessions to join", func() bool { return hub.Subscribers(path) == 2 })

	// Append until the shared reader, which seeks to the end first, delivers.
	waitForShared(t, "the shared reader to deliver", func() bool {
		appendSharedReadFile(t, path, "sync\n")
		time.Sleep(20 * time.Millisecond)
		return strings.Contains(first.received(), "sync") && strings.Contains(second.received(), "sync")
	})
	appendSharedReadFile(t, path, "payload line\n")
	waitForShared(t, "the payload in both sessions", func() bool {
		return strings.Contains(first.received(), "payload line") && strings.Contains(second.received(), "payload line")
	})
	if strings.Contains(first.received(), "existing") {
		t.Error("a shared follow read delivered a line from before the read started")
	}

	cancelFirst()
	<-firstDone
	if got := hub.Subscribers(path); got != 1 {
		t.Errorf("subscribers after one session ended = %d, want 1", got)
	}
	cancelSecond()
	<-secondDone
	if got := hub.Subscribers(path); got != 0 {
		t.Errorf("subscribers after both sessions ended = %d, want 0", got)
	}
}

func TestDeniedReadTargetNeverReachesTheHub(t *testing.T) {
	path := writeSharedReadFile(t, "secret\n")
	server := newSharedReadTestServer(t, nil)
	server.denied[path] = true
	cmd := newReadCommandWithDependencies(server.readCommandDependencies(), omode.TailClient, nil)
	var calls int32
	cmd.followShared = func(context.Context, readhub.Session) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	server.AddPendingFiles(1)

	cmd.readFileIfPermissions(context.Background(), lcontext.LContext{}, path, path, regex.NewNoop())

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("a session denied by PrepareReadTarget reached the shared reader %d times", got)
	}
	if strings.Contains(server.received(), "secret") {
		t.Error("a denied session received file content")
	}
}

func TestSharedReadEndings(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantPanic bool
		// wantExisting: the read went on from the file's beginning, as the
		// private loop does after a processor error.
		wantExisting bool
		// wantFailure: the client was told that the read failed, as the
		// private loop tells it after a failed read iteration.
		wantFailure bool
	}{
		{"reader panic", fmt.Errorf("%w: %w: boom", readhub.ErrReaderFailed, fs.ErrReaderWorkerPanic),
			true, false, false},
		{"processor error", errors.New("processor failed"), false, true, true},
		{"max count stop", readhub.ErrStopped, false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSharedReadFile(t, "existing\n")
			server := newSharedReadTestServer(t, nil)
			cmd := newReadCommandWithDependencies(server.readCommandDependencies(), omode.TailClient, nil)
			cmd.followShared = func(context.Context, readhub.Session) error { return tt.err }
			target, _ := server.PrepareReadTarget(path)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			panicked := make(chan any, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { panicked <- recover() }()
				cmd.read(ctx, lcontext.LContext{}, path, &target, "glob", regex.NewNoop())
			}()

			if tt.wantPanic {
				<-done
				if recovered := <-panicked; recovered == nil {
					t.Fatal("a shared reader panic did not panic the session like a private one")
				}
				return
			}
			if tt.wantExisting {
				waitForShared(t, "the file from its beginning", func() bool {
					return strings.Contains(server.received(), "existing")
				})
			} else {
				waitForShared(t, "the private reader to deliver appended lines", func() bool {
					appendSharedReadFile(t, path, "appended\n")
					time.Sleep(20 * time.Millisecond)
					return strings.Contains(server.received(), "appended")
				})
				if strings.Contains(server.received(), "existing") {
					t.Error("the private read after a failed shared reader did not start at the end of the file")
				}
			}
			var wantFailures []string
			if tt.wantFailure {
				wantFailures = []string{protocol.HiddenCommandFailedPrefix + readFailureReadingFile}
			}
			if got := server.failureMessages(); !slices.Equal(got, wantFailures) {
				t.Errorf("failed command messages = %q, want %q", got, wantFailures)
			}
			cancel()
			<-done
			if recovered := <-panicked; recovered != nil {
				t.Fatalf("the session panicked: %v", recovered)
			}
		})
	}
}

// failOnceLineWriter is a session output that fails on one line, like a
// client connection that broke for a moment, and delivers every other line.
// The failed line ends the read's iteration, in the private read loop as in
// the shared read, and both read the file again, so no line is lost.
type failOnceLineWriter struct {
	LineWriter
	failed *atomic.Bool
}

func (w failOnceLineWriter) WriteLineData(content []byte, lineNum uint64, sourceID string) error {
	if w.failed.CompareAndSwap(false, true) {
		return errors.New("line writer failed")
	}
	return w.LineWriter.WriteLineData(content, lineNum, sourceID)
}

// TestSharedAndPrivateReadReportTheSameFailure checks the output parity of
// the failure report: a tail session whose output fails once sends the same
// hidden failed-command message with a shared reader as with a private one.
func TestSharedAndPrivateReadReportTheSameFailure(t *testing.T) {
	report := func(t *testing.T, shared bool) []string {
		t.Helper()
		var hub *readhub.Hub
		if shared {
			hub = NewReadHub(&config.ServerConfig{ReadRetryIntervalMs: 10}, logging.NopLogger{})
		}
		path := writeSharedReadFile(t, "existing\n")
		server := newSharedReadTestServer(t, hub)
		dependencies := server.readCommandDependencies()
		writers, failed := dependencies.newLineWriter, &atomic.Bool{}
		dependencies.newLineWriter = func(ctx context.Context, generation uint64) LineWriter {
			return failOnceLineWriter{LineWriter: writers(ctx, generation), failed: failed}
		}
		cmd := newReadCommandWithDependencies(dependencies, omode.TailClient, nil)
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
		// Both reads start at the end of the file, so append until a line
		// reaches the output. The first one fails the writer, and both paths
		// report that before they read the file again, which delivers it.
		waitForShared(t, "the read to go on after the failed line", func() bool {
			appendSharedReadFile(t, path, "payload\n")
			time.Sleep(20 * time.Millisecond)
			return strings.Contains(server.received(), "payload")
		})
		cancel()
		<-done
		if !failed.Load() {
			t.Fatal("test setup: the session's output never failed")
		}
		return server.failureMessages()
	}

	private := report(t, false)
	want := []string{protocol.HiddenCommandFailedPrefix + readFailureReadingFile}
	if !slices.Equal(private, want) {
		t.Fatalf("private read failure messages = %q, want %q", private, want)
	}
	if got := report(t, true); !slices.Equal(got, private) {
		t.Errorf("shared read failure messages = %q, want the private read's %q", got, private)
	}
}
