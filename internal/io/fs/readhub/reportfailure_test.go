package readhub

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

// The hub tells a session about every failed read it handles itself, so that
// the session's client hears of it as it does for a private read, whose read
// command reports every failed iteration of its retry loop (see
// handlers.executeReadLoop).

// deliversTo appends lines until f got one, so the test knows the session
// reads on after the failure.
func deliversTo(t *testing.T, file *testFile, f *follower, text string) {
	t.Helper()
	waitFor(t, "the session to read on after the failure", func() bool {
		file.appendLines(text)
		time.Sleep(20 * time.Millisecond)
		return f.recorder.hasLine(text)
	})
}

// TestFollowReportsAFailedReadOfTheSharedReader checks that every session of
// a shared reader whose read failed is told, as the read command tells the
// client of a private reader before it reads the file again.
func TestFollowReportsAFailedReadOfTheSharedReader(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	healthy := hub.seams.startReader
	var reads atomic.Int64
	release := make(chan struct{})
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		if reads.Add(1) == 1 {
			<-release // let both sessions join first
			return errors.New("injected read failure")
		}
		return healthy(ctx, reader, processor)
	}

	followers := []*follower{
		startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "a"),
		startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "b"),
	}
	waitFor(t, "both sessions to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	close(release)

	for i, f := range followers {
		waitFor(t, "the failure report of session", func() bool { return f.failures.Load() > 0 })
		if got := f.failures.Load(); got != 1 {
			t.Errorf("session %d got %d failure reports, want 1 for one failed read", i, got)
		}
		// The shared reader read the file again, as a private one does.
		deliversTo(t, file, f, "after the failed read")
	}
}

// TestFollowReportsASharedReaderThatFailedWithoutAPanic checks the report of
// the failure that hands a session over to a private reader: a private
// reader failing that way ends its read command's iteration, which reports
// it.
func TestFollowReportsASharedReaderThatFailedWithoutAPanic(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	f := syncReader(t, hub, file)
	shared := hub.entryFor(file.path)
	if shared == nil {
		t.Fatal("no shared read of the file")
	}

	shared.fail(errors.New("injected failure"))

	waitFor(t, "the failure report", func() bool { return f.failures.Load() > 0 })
	if got := f.failures.Load(); got != 1 {
		t.Errorf("session got %d failure reports, want 1 for one failed shared reader", got)
	}
	// The session goes on with a private reader of its own.
	deliversTo(t, file, f, "after the failed shared reader")
}

// TestPrivateReadAfterAnEvictionReportsItsFailedReads checks the report of
// an evicted session's own reader, which is the read command's retry loop
// for that session and reports what that loop reports.
func TestPrivateReadAfterAnEvictionReportsItsFailedReads(t *testing.T) {
	file := newTestFile(t)
	rec := &recorder{failAt: "boom"}
	session, _ := messageSession(t, file.path, rec)
	var failures atomic.Int64
	session.ReportFailure = func() { failures.Add(1) }
	reading := newSessionRead(session, logging.NopLogger{},
		Options{RetryInterval: 10 * time.Millisecond}, unknownPosition(), joinSkip{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reading.readPrivately(ctx, nil) }()
	// The read owns the filter and the processor until it returned, also
	// when the test failed meanwhile.
	t.Cleanup(func() {
		cancel()
		<-done
		reading.close()
	})
	waitFor(t, "the failure report of the private read", func() bool {
		file.appendLines("boom")
		time.Sleep(20 * time.Millisecond)
		return failures.Load() > 0
	})
	if !rec.hasLine("boom") {
		t.Error("the private read reported a failure without processing the failing line")
	}
}
