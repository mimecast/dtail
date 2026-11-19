//go:build linux

package handlers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

type journalReadTestServer struct {
	catLimiter    chan struct{}
	tailLimiter   chan struct{}
	outputLines   chan []byte
	serverMessage chan string
	prepared      []string
	pending       int32
	shutdowns     int32
}

func newJournalReadTestServer() *journalReadTestServer {
	return &journalReadTestServer{
		catLimiter:    make(chan struct{}, 1),
		tailLimiter:   make(chan struct{}, 1),
		outputLines:   make(chan []byte, 16),
		serverMessage: make(chan string, 16),
	}
}

func (s *journalReadTestServer) LogContext() any {
	return "journal-read-test"
}

func (s *journalReadTestServer) Logger() logging.Logger {
	return logging.NopLogger{}
}

func (s *journalReadTestServer) ReaderLogger() logging.Logger {
	return logging.NopLogger{}
}

func (s *journalReadTestServer) PrepareReadTarget(path string) (fs.ValidatedReadTarget, bool) {
	s.prepared = append(s.prepared, path)
	target, err := fs.NewValidatedJournalTarget(path)
	return target, err == nil
}

func (s *journalReadTestServer) readCommandDependencies() readCommandDependencies {
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
			maxGlobTargets:            1000,
		},
		newLineWriter: func(ctx context.Context, generation uint64) LineWriter {
			return NewNetworkWriter(ctx, s.outputLines, s.serverMessage, "testhost", false, false,
				generation, func() uint64 { return 0 }, s.Logger())
		},
	}
}

func (s *journalReadTestServer) AcquireReadSlot(ctx context.Context, mode omode.Mode, _ string) (func(), bool) {
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

func (s *journalReadTestServer) SendReadMessage(ctx context.Context, generation uint64, message string) {
	select {
	case s.serverMessage <- encodeGeneratedMessage(generation, message+"\n"):
	case <-ctx.Done():
	}
}

func (s *journalReadTestServer) NewReadMessages(ctx context.Context, generation uint64) (chan string, func()) {
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

func (*journalReadTestServer) FinishReadBatch(context.Context, omode.Mode, uint64) {}

func (*journalReadTestServer) DebugReadLifecycle(string, ...any) {}

func (s *journalReadTestServer) Aggregate() *mapaggregate.Aggregate {
	return nil
}

func (s *journalReadTestServer) AddPendingFiles(delta int32) int32 {
	return atomic.AddInt32(&s.pending, delta)
}

func (s *journalReadTestServer) CompletePendingFile() (int32, int32) {
	return atomic.AddInt32(&s.pending, -1), 0
}

func (s *journalReadTestServer) PendingAndActive() (int32, int32) {
	return atomic.LoadInt32(&s.pending), 0
}

func (s *journalReadTestServer) TriggerShutdown(context.Context) {
	atomic.AddInt32(&s.shutdowns, 1)
}

var _ readCommandServer = (*journalReadTestServer)(nil)
var _ readCommandDependencyProvider = (*journalReadTestServer)(nil)

func TestReadCommandDispatchesJournalSpecWithoutGlob(t *testing.T) {

	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{"alpha"},
		},
	})

	server := newJournalReadTestServer()
	command := newReadCommand(server, omode.CatClient)
	command.Start(
		context.Background(),
		emptyLContext(),
		3,
		[]string{"cat", "journal:ssh.service", ""},
		1,
	)

	if got, want := server.prepared, []string{"journal:ssh.service"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("prepared targets = %q, want %q", got, want)
	}
	if got := strings.TrimSpace(mock.Args(t)); got != "-u ssh.service" {
		t.Fatalf("journalctl args = %q, want %q", got, "-u ssh.service")
	}

	// Output is now the one and only read path, so journal output is delivered as
	// protocol-formatted bytes on the output channel rather than line objects on
	// the shared lines channel. The payload still carries the journal sourceID
	// and the line content, which is the real regression coverage here.
	got := waitForOutputLine(t, server.outputLines)
	if !strings.Contains(got, "journal:ssh.service") {
		t.Fatalf("output output missing journal sourceID; got %q", got)
	}
	if !strings.Contains(got, "alpha") {
		t.Fatalf("output output missing line content %q; got %q", "alpha", got)
	}
}

func TestReadCommandPassesJournalUnitAsSingleArgWithoutShell(t *testing.T) {

	marker := filepath.Join(t.TempDir(), "journal-injection-marker")
	unit := "ssh.service;touch${IFS}" + marker
	spec := fs.JournalSpecPrefix + unit

	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			unit: {
				Lines: []string{"safe"},
			},
		},
	})

	server := newJournalReadTestServer()
	command := newReadCommand(server, omode.CatClient)
	command.Start(
		context.Background(),
		emptyLContext(),
		3,
		[]string{"cat", spec, ""},
		1,
	)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("journal unit was interpreted as shell; marker stat error = %v", err)
	}

	unitFile, err := os.ReadFile(mock.UnitFile)
	if err != nil {
		t.Fatalf("read mock unit file: %v", err)
	}
	if got := strings.TrimSpace(string(unitFile)); got != unit {
		t.Fatalf("journalctl unit arg = %q, want %q", got, unit)
	}

	// See the note in TestReadCommandDispatchesJournalSpecWithoutGlob: output is
	// the only read path now, so the line content arrives as protocol-formatted
	// bytes on the output channel.
	got := waitForOutputLine(t, server.outputLines)
	if !strings.Contains(got, "safe") {
		t.Fatalf("output output missing line content %q; got %q", "safe", got)
	}
}

// TestServerHandlerShutdownTerminatesMapFollowJournalWithoutOutputReader covers
// the full abrupt-disconnect path with a real ServerHandler and journalctl child.
// The queued interval result proves the child and Processor are active;
// no goroutine consumes handler output before Shutdown.
func TestServerHandlerShutdownTerminatesMapFollowJournalWithoutOutputReader(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{testStatsLine},
		},
	})
	handler := newMapTestHandler(t)
	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"journal:ssh.service"},
		Query: "from STATS select count($time),$time group by $time interval 1",
		Regex: ".",
	}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}

	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	if _, err := handler.Write([]byte(frames.String())); err != nil {
		t.Fatalf("write commands: %v", err)
	}

	waitForHandlerCondition(t, 5*time.Second, "active journal processor did not queue an aggregate result", func() bool {
		return len(handler.maprMessages) > 0
	}, func() string {
		return fmt.Sprintf("map result queue length=%d", len(handler.maprMessages))
	})
	if got := len(handler.tailLimiter); got != 1 {
		t.Fatalf("tail limiter occupancy before shutdown = %d, want 1", got)
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.Shutdown()
	}()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler Shutdown hung waiting for journal MapReduce follow processor")
	}

	mock.WaitForTerm(t, time.Second)
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("handler did not quiesce: pending=%d active=%d", pending, active)
	}
	if got := len(handler.tailLimiter); got != 0 {
		t.Fatalf("tail limiter occupancy after shutdown = %d, want 0", got)
	}
	select {
	case <-handler.Done():
	default:
		t.Fatal("handler did not signal transport shutdown")
	}
}

func emptyLContext() lcontext.LContext {
	return lcontext.LContext{}
}

// waitForOutputLine drains one payload from the output channel used by the
// single (former "output") read path. Session generation is 0 in these tests,
// so encodeGeneratedBytes leaves the payload unmodified and it can be compared
// as-is.
func waitForOutputLine(t *testing.T, outputLines <-chan []byte) string {
	t.Helper()
	select {
	case payload := <-outputLines:
		return string(payload)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for journal output")
		return ""
	}
}
