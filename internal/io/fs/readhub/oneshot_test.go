package readhub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// testMember is one session reading the test file through a group read.
type testMember struct {
	recorder *recorder
	cancel   context.CancelFunc
	done     chan error
	messages chan string
}

func newGroupHub(wait time.Duration) *Hub {
	return New(Options{Logger: logging.NopLogger{}, GroupWait: wait})
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "once.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validatedTarget(t *testing.T, path string) fs.ValidatedReadTarget {
	t.Helper()
	target, err := fs.NewValidatedReadTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func groupSession(t *testing.T, path string, ltx lcontext.LContext, re regex.Regex,
	newProcessor func() line.Processor, messages chan string) Session {

	t.Helper()
	return Session{
		Target:         validatedTarget(t, path),
		FilePath:       path,
		GlobID:         "glob",
		LContext:       ltx,
		Regex:          re,
		ServerMessages: messages,
		Logger:         textLogger{},
		NewProcessor:   newProcessor,
	}
}

func startGroupMember(t *testing.T, hub *Hub, mode omode.Mode, path string, ltx lcontext.LContext,
	re regex.Regex, group Group) *testMember {

	t.Helper()
	return startSlottedMember(t, hub, mode, path, ltx, re, group, freeSlot)
}

// startSlottedMember is startGroupMember with the member's slot acquirer.
func startSlottedMember(t *testing.T, hub *Hub, mode omode.Mode, path string, ltx lcontext.LContext,
	re regex.Regex, group Group, acquire SlotAcquirer) *testMember {

	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := &testMember{recorder: &recorder{}, cancel: cancel, done: make(chan error, 1),
		messages: make(chan string, 16)}
	session := groupSession(t, path, ltx, re, m.recorder.newProcessor, m.messages)
	go func() { m.done <- hub.ReadOnce(ctx, mode, session, group, acquire) }()
	t.Cleanup(func() {
		cancel()
		<-m.done
		m.done <- nil
	})
	return m
}

// wait returns what ReadOnce returned.
func (m *testMember) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-m.done:
		m.done <- err // for the cleanup
		return err
	case <-time.After(waitTimeout):
		t.Fatal("ReadOnce did not return")
		return nil
	}
}

func groupMembers(hub *Hub, path string) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	count := 0
	for key, e := range hub.oneshot.entries {
		if key.path == path {
			count += len(e.snapshot())
		}
	}
	return count
}

// privateSnapshot reads path with a private reader in mode and returns what
// its processor got.
func privateSnapshot(t *testing.T, mode omode.Mode, path string, ltx lcontext.LContext,
	re regex.Regex) *recorder {

	t.Helper()
	return privateSnapshotSplitAt(t, mode, path, ltx, re, 0)
}

// privateSnapshotSplitAt is privateSnapshot with a maximum line length.
func privateSnapshotSplitAt(t *testing.T, mode omode.Mode, path string, ltx lcontext.LContext,
	re regex.Regex, maxLineLength int) *recorder {

	t.Helper()
	private := &recorder{}
	target := validatedTarget(t, path)
	reader, err := fs.NewReadFile(fs.ReadOptions{
		Mode: mode, Target: &target, FilePath: path, GlobID: "glob", Logger: logging.NopLogger{},
		MaxLineLength: maxLineLength,
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := private.newProcessor()
	if err := reader.Start(context.Background(), ltx, processor, re); err != nil {
		t.Fatal(err)
	}
	if err := processor.Close(); err != nil {
		t.Fatal(err)
	}
	return private
}

type lineEvent struct {
	text    string
	lineNum uint64
	source  string
}

func snapshotLineEvents(r *recorder) []lineEvent {
	var events []lineEvent
	for _, e := range r.snapshot() {
		if e.kind == "line" {
			events = append(events, lineEvent{e.text, e.lineNum, e.source})
		}
	}
	return events
}

func TestReadOnceMatchesPrivateSnapshotRead(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 3000; i++ {
		switch {
		case i%97 == 0:
			content.WriteString("\n") // empty lines reach the processor, too
		case i%7 == 0:
			fmt.Fprintf(&content, "ERROR %d %s\n", i, strings.Repeat("e", i%50))
		default:
			fmt.Fprintf(&content, "INFO %d %s\n", i, strings.Repeat("i", i%80))
		}
	}
	content.WriteString("ERROR last line without a newline")
	path := writeFile(t, content.String())

	re := mustRegex(t, "ERROR")
	inverted, err := regex.New("ERROR", regex.Invert)
	if err != nil {
		t.Fatal(err)
	}
	reads := []struct {
		name string
		ltx  lcontext.LContext
		re   regex.Regex
	}{
		{"everything", lcontext.LContext{}, regex.NewNoop()},
		{"grep", lcontext.LContext{}, re},
		{"inverted", lcontext.LContext{}, inverted},
		{"before", lcontext.LContext{BeforeContext: 2}, re},
		{"after", lcontext.LContext{AfterContext: 3}, re},
		{"before and after", lcontext.LContext{BeforeContext: 1, AfterContext: 2}, re},
		{"max", lcontext.LContext{MaxCount: 5}, re},
		{"max with after", lcontext.LContext{MaxCount: 3, AfterContext: 2}, re},
		{"max everything", lcontext.LContext{MaxCount: 10}, regex.NewNoop()},
	}

	for _, mode := range []omode.Mode{omode.CatClient, omode.GrepClient} {
		t.Run(mode.String(), func(t *testing.T) {
			// A small queue makes the reader wait for the members.
			hub := New(Options{Logger: logging.NopLogger{}, GroupWait: time.Hour, QueueChunks: 1})
			group := Group{ID: "all-" + mode.String(), Members: len(reads)}
			members := make([]*testMember, len(reads))
			for i, read := range reads {
				members[i] = startGroupMember(t, hub, mode, path, read.ltx, read.re, group)
			}
			for i, read := range reads {
				if err := members[i].wait(t); err != nil {
					t.Errorf("%s: ReadOnce() = %v", read.name, err)
				}
				got := snapshotLineEvents(members[i].recorder)
				want := snapshotLineEvents(privateSnapshot(t, mode, path, read.ltx, read.re))
				if len(want) == 0 {
					t.Fatalf("%s: the private read saw no lines", read.name)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: shared read got %d lines, private %d; first shared %v, first private %v",
						read.name, len(got), len(want), head(got), head(want))
				}
				if closes := countKind(members[i].recorder, "close"); closes != 1 {
					t.Errorf("%s: processor closed %d times, want 1", read.name, closes)
				}
			}
		})
	}
}

func head(events []lineEvent) []lineEvent {
	return events[:min(3, len(events))]
}

func countKind(r *recorder, kind string) int {
	count := 0
	for _, e := range r.snapshot() {
		if e.kind == kind {
			count++
		}
	}
	return count
}

// countingSeams counts the reads a hub starts and lets a test hold them.
type countingSeams struct {
	mu      sync.Mutex
	started int
	release chan struct{}
}

func (c *countingSeams) install(hub *Hub) {
	healthy := hub.seams.startReader
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		c.mu.Lock()
		c.started++
		c.mu.Unlock()
		if c.release != nil {
			<-c.release
		}
		return healthy(ctx, reader, processor)
	}
}

func (c *countingSeams) starts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

func TestReadOnceReadsOnceForTheWholeGroup(t *testing.T) {
	path := writeFile(t, "a\nb\nc\n")
	hub := newGroupHub(time.Hour)
	seams := &countingSeams{}
	seams.install(hub)

	group := Group{ID: "g", Members: 3}
	var members []*testMember
	for range group.Members {
		members = append(members, startGroupMember(t, hub, omode.CatClient, path,
			lcontext.LContext{}, regex.NewNoop(), group))
	}
	for _, m := range members {
		if err := m.wait(t); err != nil {
			t.Fatalf("ReadOnce() = %v", err)
		}
		if got, want := m.recorder.lines(), []string{"a\n", "b\n", "c\n"}; !reflect.DeepEqual(got, want) {
			t.Errorf("lines = %q, want %q", got, want)
		}
	}
	if starts := seams.starts(); starts != 1 {
		t.Errorf("the group read the file %d times, want once", starts)
	}
	if groupMembers(hub, path) != 0 {
		t.Error("the hub still holds the finished group read")
	}
}

func TestReadOnceStartsAfterTheGroupWaitWithTheMembersItHas(t *testing.T) {
	path := writeFile(t, "a\nb\n")
	const wait = 200 * time.Millisecond
	hub := newGroupHub(wait)

	began := time.Now()
	member := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(),
		Group{ID: "g", Members: 3})
	if err := member.wait(t); err != nil {
		t.Fatalf("ReadOnce() = %v", err)
	}
	if elapsed := time.Since(began); elapsed < wait {
		t.Errorf("the read started after %v, before the group wait of %v", elapsed, wait)
	}
	if got, want := member.recorder.lines(), []string{"a\n", "b\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestReadOnceLateJoinerReadsPrivately(t *testing.T) {
	path := writeFile(t, "a\n")
	hub := newGroupHub(time.Hour)
	seams := &countingSeams{release: make(chan struct{})}
	seams.install(hub)

	group := Group{ID: "g", Members: 1}
	first := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group)
	waitFor(t, "the group read to start", func() bool { return seams.starts() == 1 })

	late := &recorder{}
	session := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), late.newProcessor, nil)
	if err := hub.ReadOnce(context.Background(), omode.CatClient, session, group, freeSlot); !errors.Is(err, ErrGroupReadStarted) {
		t.Fatalf("late ReadOnce() = %v, want ErrGroupReadStarted", err)
	}
	if events := late.snapshot(); len(events) != 0 {
		t.Errorf("the late session got %v, want nothing", events)
	}
	close(seams.release)
	if err := first.wait(t); err != nil {
		t.Fatalf("ReadOnce() = %v", err)
	}

	// The same group ID in another mode or for another file is another read.
	other := startGroupMember(t, hub, omode.GrepClient, path, lcontext.LContext{}, regex.NewNoop(), group)
	if err := other.wait(t); err != nil {
		t.Fatalf("ReadOnce() in grep mode = %v", err)
	}
	if got := other.recorder.lines(); !reflect.DeepEqual(got, []string{"a\n"}) {
		t.Errorf("grep mode lines = %q", got)
	}
}

func TestReadOnceCancelledMemberReleasesTheGroup(t *testing.T) {
	var content strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	path := writeFile(t, content.String())
	hub := New(Options{Logger: logging.NopLogger{}, GroupWait: time.Hour, QueueChunks: 1})
	group := Group{ID: "g", Members: 2}

	// The stuck member takes its first line and then waits for its context,
	// so the reader soon waits for it.
	stuckCtx, cancelStuck := context.WithCancel(context.Background())
	stuck := &blockingRecorder{ctx: stuckCtx, blocked: make(chan struct{})}
	stuckDone := make(chan error, 1)
	session := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), stuck.newProcessor, nil)
	go func() { stuckDone <- hub.ReadOnce(stuckCtx, omode.CatClient, session, group, freeSlot) }()

	healthy := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group)
	<-stuck.blocked
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-healthy.done:
		t.Fatalf("the healthy member finished while the other one blocked the read: %v", err)
	default:
	}

	cancelStuck()
	select {
	case err := <-stuckDone:
		if err != nil {
			t.Errorf("cancelled ReadOnce() = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the cancelled member did not return")
	}
	if err := healthy.wait(t); err != nil {
		t.Fatalf("ReadOnce() = %v", err)
	}
	if got := len(healthy.recorder.lines()); got != 20000 {
		t.Errorf("the healthy member got %d lines, want 20000", got)
	}
}

// blockingRecorder's processor blocks on its first line until ctx ends.
type blockingRecorder struct {
	ctx     context.Context
	blocked chan struct{}
	once    sync.Once
}

func (b *blockingRecorder) newProcessor() line.Processor { return b }

func (b *blockingRecorder) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	pool.RecycleBytesBuffer(buf)
	b.once.Do(func() {
		close(b.blocked)
		<-b.ctx.Done()
	})
	return nil
}

func (b *blockingRecorder) Flush() error { return nil }
func (b *blockingRecorder) Close() error { return nil }

func TestReadOnceGroupLeftBeforeTheStartNeverReads(t *testing.T) {
	path := writeFile(t, "a\n")
	hub := newGroupHub(time.Hour)
	seams := &countingSeams{}
	seams.install(hub)

	member := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(),
		Group{ID: "g", Members: 2})
	waitFor(t, "the member to join", func() bool { return groupMembers(hub, path) == 1 })
	hub.mu.Lock()
	var e *groupEntry
	for _, candidate := range hub.oneshot.entries {
		e = candidate
	}
	hub.mu.Unlock()

	member.cancel()
	if err := member.wait(t); err != nil {
		t.Errorf("ReadOnce() = %v, want nil", err)
	}
	select {
	case <-e.done:
	case <-time.After(waitTimeout):
		t.Fatal("the abandoned group read did not stop")
	}
	if starts := seams.starts(); starts != 0 {
		t.Errorf("the abandoned group read the file %d times", starts)
	}
	if groupMembers(hub, path) != 0 {
		t.Error("the hub still holds the abandoned group read")
	}
}

func TestReadOnceReportsReaderFailuresToEveryMember(t *testing.T) {
	errRead := errors.New("injected read error")
	tests := []struct {
		name  string
		start func(context.Context, *fs.ReadFile, line.Processor) error
		want  error
	}{
		{"error", func(context.Context, *fs.ReadFile, line.Processor) error { return errRead }, errRead},
		{"panic", func(context.Context, *fs.ReadFile, line.Processor) error { panic("injected reader bug") },
			fs.ErrReaderWorkerPanic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, "a\n")
			hub := newGroupHub(time.Hour)
			hub.seams.startReader = tt.start
			group := Group{ID: "g", Members: 2}
			members := []*testMember{
				startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group),
				startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group),
			}
			for _, m := range members {
				if err := m.wait(t); !errors.Is(err, tt.want) {
					t.Errorf("ReadOnce() = %v, want %v", err, tt.want)
				}
			}
		})
	}
}

func TestReadOnceWarnsEveryMemberAboutLongLines(t *testing.T) {
	path := writeFile(t, "short\n"+strings.Repeat("x", 40)+"\nshort again\n")
	hub := New(Options{Logger: textLogger{}, GroupWait: time.Hour, MaxLineLength: 16})
	group := Group{ID: "g", Members: 2}
	members := []*testMember{
		startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group),
		startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group),
	}
	want := snapshotLineEvents(privateSnapshotSplitAt(t, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), 16))
	for _, m := range members {
		if err := m.wait(t); err != nil {
			t.Fatalf("ReadOnce() = %v", err)
		}
		if got := snapshotLineEvents(m.recorder); !reflect.DeepEqual(got, want) {
			t.Errorf("lines = %v, want %v", got, want)
		}
		select {
		case message := <-m.messages:
			if !strings.Contains(message, "Long log line") || !strings.Contains(message, path) {
				t.Errorf("message = %q, want the long line warning for %s", message, path)
			}
		default:
			t.Error("a member did not get the long line warning")
		}
	}
}

func TestReadOnceRejectsInvalidReads(t *testing.T) {
	path := writeFile(t, "a\n")
	hub := newGroupHub(time.Hour)
	valid := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), (&recorder{}).newProcessor, nil)
	tests := []struct {
		name    string
		mode    omode.Mode
		session Session
		group   Group
	}{
		{"tail mode", omode.TailClient, valid, Group{ID: "g", Members: 1}},
		{"no group ID", omode.CatClient, valid, Group{Members: 1}},
		{"no members", omode.CatClient, valid, Group{ID: "g"}},
		{"no target", omode.CatClient, Session{FilePath: path, NewProcessor: valid.NewProcessor},
			Group{ID: "g", Members: 1}},
		{"no slot acquirer", omode.CatClient, valid, Group{ID: "g", Members: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acquire := SlotAcquirer(freeSlot)
			if tt.name == "no slot acquirer" {
				acquire = nil
			}
			err := hub.ReadOnce(context.Background(), tt.mode, tt.session, tt.group, acquire)
			if err == nil || errors.Is(err, ErrGroupReadStarted) {
				t.Errorf("ReadOnce() = %v, want a validation error", err)
			}
		})
	}
}

// freeSlot is a slot acquirer with unlimited slots.
var freeSlot SlotAcquirer = freeSlots{}

type freeSlots struct{}

func (freeSlots) AcquireSlot(context.Context) (func(), bool) { return func() {}, true }
func (freeSlots) TryAcquireSlot() (func(), bool)             { return func() {}, true }

// A group read never evicts a member that falls behind, as a shared follow
// read does: delivery blocks until the slow member took the item, and every
// member still gets exactly the lines of a private snapshot read, numbered
// from 1, even with a queue of one chunk.
func TestReadOnceSlowMemberIsNotEvicted(t *testing.T) {
	var content strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	path := writeFile(t, content.String())
	hub := New(Options{Logger: logging.NopLogger{}, GroupWait: time.Hour, QueueChunks: 1})

	group := Group{ID: "g", Members: 2}
	fast := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group)

	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	slow := &testMember{recorder: &recorder{}, done: make(chan error, 1)}
	session := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), func() line.Processor {
		return gatedProcessor{Processor: slow.recorder.newProcessor(), gate: gate, entered: entered}
	}, nil)
	go func() { slow.done <- hub.ReadOnce(context.Background(), omode.CatClient, session, group, freeSlot) }()

	// Hold the slow member back long enough for a follow read to evict it.
	<-entered
	time.Sleep(100 * time.Millisecond)
	close(gate)

	want := snapshotLineEvents(privateSnapshot(t, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop()))
	for name, m := range map[string]*testMember{"fast": fast, "slow": slow} {
		if err := m.wait(t); err != nil {
			t.Fatalf("%s ReadOnce() = %v", name, err)
		}
		if got := snapshotLineEvents(m.recorder); !reflect.DeepEqual(got, want) {
			t.Errorf("%s member got %d lines, want the %d of a private read", name, len(got), len(want))
		}
	}
}
