package handlers

// Tests for the glob expansion cap (MaxGlobTargets).
//
// Security context: an authenticated user with a broad read permission (e.g.
// "readfiles:^/.*") can craft a glob that matches thousands of paths. Before
// this fix, filepath.Glob would return every match, pendingFiles would be
// incremented by the full count, and one goroutine would be spawned per path
// — all before any concurrency limiter was consulted. The cap enforced here
// truncates the expansion to MaxGlobTargets paths so that the number of
// goroutines and the memory used by a single read command are bounded.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// globCapTestServer supplies the focused read-command roles while recording
// PrepareReadTarget invocations. Its limiter is generously sized so it never
// blocks these tests.
type globCapTestServer struct {
	catLimiter    chan struct{}
	tailLimiter   chan struct{}
	serverMessage chan string
	// preparedCount counts how many file paths were dispatched for reading.
	preparedCount int32
	// pendingFiles mirrors the lifecycle counter used by readFiles.
	pendingFiles int32
	// maxGlobTargets is copied into the command's immutable read timings.
	maxGlobTargets int
}

func newGlobCapTestServer(maxGlobTargets int) *globCapTestServer {
	// Use a large limiter so reads are never blocked by concurrency limits
	// — we only want to test the glob cap, not the semaphore.
	limiter := make(chan struct{}, 10000)
	return &globCapTestServer{
		catLimiter:     limiter,
		tailLimiter:    limiter,
		serverMessage:  make(chan string, 128),
		maxGlobTargets: maxGlobTargets,
	}
}

// PrepareReadTarget records the path and returns a valid journal-free target.
// We use a plain ValidatedReadTarget with FileKind so no actual file I/O is
// attempted; the goroutine that calls read() will fail gracefully when the
// nonexistent catFile cannot be opened, but that is fine for this test.
func (s *globCapTestServer) PrepareReadTarget(path string) (fs.ValidatedReadTarget, error) {
	atomic.AddInt32(&s.preparedCount, 1)
	// Return a valid file-kind target so readFileIfPermissions proceeds past
	// the permission check and reaches the actual read machinery.
	return fs.ValidatedReadTarget{Kind: fs.FileKind}, nil
}

func (s *globCapTestServer) LogContext() any              { return "glob-cap-test" }
func (s *globCapTestServer) Logger() logging.Logger       { return logging.NopLogger{} }
func (s *globCapTestServer) ReaderLogger() logging.Logger { return logging.NopLogger{} }

func (s *globCapTestServer) readCommandDependencies() readCommandDependencies {
	return readCommandDependencies{
		server:       s,
		lifecycle:    s,
		aggregates:   s,
		logger:       s.Logger(),
		readerLogger: s.ReaderLogger(),
		logContext:   s.LogContext(),
		timings: readTimings{
			globRetryInterval:         time.Millisecond,
			readRetryInterval:         time.Millisecond,
			outputEOFAckTimeout:       time.Millisecond,
			legacyAggregateInputGrace: time.Millisecond,
			maxLineLength:             1024 * 1024,
			maxGlobTargets:            s.maxGlobTargets,
		},
		newLineWriter: func(context.Context, uint64) LineWriter { return nopLineWriter{} },
	}
}

func (s *globCapTestServer) AcquireReadSlot(ctx context.Context, mode omode.Mode, _ string) (func(), bool) {
	limiter := s.tailLimiter
	if mode == omode.CatClient || mode == omode.GrepClient {
		limiter = s.catLimiter
	}
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (s *globCapTestServer) TryAcquireReadSlot(mode omode.Mode, _ string) (func(), bool) {
	limiter := s.tailLimiter
	if mode == omode.CatClient || mode == omode.GrepClient {
		limiter = s.catLimiter
	}
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, true
	default:
		return nil, false
	}
}

func (s *globCapTestServer) SendReadMessage(ctx context.Context, generation uint64, message string) {
	select {
	case s.serverMessage <- encodeGeneratedMessage(generation, message+"\n"):
	case <-ctx.Done():
	}
}

func (s *globCapTestServer) NewReadMessages(ctx context.Context, generation uint64) (chan string, func()) {
	messages := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case message, ok := <-messages:
				if !ok {
					return
				}
				select {
				case s.serverMessage <- encodeGeneratedMessage(generation, message):
				case <-ctx.Done():
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

func (*globCapTestServer) FinishReadBatch(context.Context, omode.Mode, uint64) {}

func (*globCapTestServer) DebugReadLifecycle(string, ...any) {}

func (s *globCapTestServer) Aggregate() *mapaggregate.Aggregate { return nil }

// AddPendingFiles tracks the lifecycle counter that readFiles uses to
// signal EOF; it operates on a separate field from preparedCount.
func (s *globCapTestServer) AddPendingFiles(delta int32) int32 {
	return atomic.AddInt32(&s.pendingFiles, delta)
}

func (s *globCapTestServer) CompletePendingFile() (int32, int32) {
	remaining := atomic.AddInt32(&s.pendingFiles, -1)
	return remaining, 0
}

func (s *globCapTestServer) PendingAndActive() (int32, int32) {
	return atomic.LoadInt32(&s.pendingFiles), 0
}
func (s *globCapTestServer) TriggerShutdown(context.Context) {}

// verify the interface is satisfied at compile time
var _ readCommandServer = (*globCapTestServer)(nil)
var _ readCommandLifecycle = (*globCapTestServer)(nil)
var _ readCommandDependencyProvider = (*globCapTestServer)(nil)

// createTempFiles creates n empty files named file0000.log … in dir and
// returns the glob pattern that matches all of them.
func createTempFiles(t *testing.T, dir string, n int) string {
	t.Helper()
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, fmt.Sprintf("file%04d.log", i))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatalf("create temp file %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "*.log")
}

// TestGlobCapTruncatesExcessPaths verifies that when a glob matches more files
// than MaxGlobTargets, only MaxGlobTargets paths are dispatched. The check is
// done by counting PrepareReadTarget invocations on the mock server.
func TestGlobCapTruncatesExcessPaths(t *testing.T) {

	const (
		totalFiles = 20 // files on disk — clearly above the cap
		maxTargets = 5  // deliberately low cap to prove truncation
	)

	dir := t.TempDir()
	glob := createTempFiles(t, dir, totalFiles)

	srv := newGlobCapTestServer(maxTargets)
	cmd := newReadCommand(srv, omode.CatClient)

	// Run readGlob with retries=1 so it finds files immediately.
	cmd.readGlob(context.Background(), lcontext.LContext{}, glob, regex.NewNoop(), 1)

	got := int(atomic.LoadInt32(&srv.preparedCount))
	if got > maxTargets {
		t.Fatalf("glob cap not enforced: dispatched %d paths, expected at most %d", got, maxTargets)
	}
	if got == 0 {
		t.Fatal("no paths were dispatched; expected exactly cap paths to be served")
	}
}

// TestGlobCapUnderLimitPassesAll verifies that when the number of glob matches
// is at or below MaxGlobTargets, all paths are dispatched without truncation.
func TestGlobCapUnderLimitPassesAll(t *testing.T) {

	const (
		totalFiles = 5
		maxTargets = 10 // cap is above totalFiles — nothing should be dropped
	)

	dir := t.TempDir()
	glob := createTempFiles(t, dir, totalFiles)

	srv := newGlobCapTestServer(maxTargets)
	cmd := newReadCommand(srv, omode.CatClient)

	cmd.readGlob(context.Background(), lcontext.LContext{}, glob, regex.NewNoop(), 1)

	got := int(atomic.LoadInt32(&srv.preparedCount))
	if got != totalFiles {
		t.Fatalf("expected all %d paths dispatched, got %d", totalFiles, got)
	}
}

// TestGlobCapExactlyAtLimit verifies that when the number of glob matches
// equals MaxGlobTargets exactly, all paths are dispatched (boundary condition).
func TestGlobCapExactlyAtLimit(t *testing.T) {

	const count = 8 // cap == totalFiles

	dir := t.TempDir()
	glob := createTempFiles(t, dir, count)

	srv := newGlobCapTestServer(count)
	cmd := newReadCommand(srv, omode.CatClient)

	cmd.readGlob(context.Background(), lcontext.LContext{}, glob, regex.NewNoop(), 1)

	got := int(atomic.LoadInt32(&srv.preparedCount))
	if got != count {
		t.Fatalf("expected exactly %d paths dispatched (no truncation at limit), got %d", count, got)
	}
}
