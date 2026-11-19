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
	"sync/atomic"
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

const waitTimeout = 10 * time.Second

// event is one call a recordingProcessor saw.
type event struct {
	kind    string // "line", "restart", "flush", "close"
	text    string
	lineNum uint64
	source  string
	id      int
}

// recorder collects the events of every processor a session made, in order.
type recorder struct {
	mu     sync.Mutex
	events []event
	made   int
	// failAt makes ProcessLine fail on the line with this text.
	failAt string
}

var errProcessor = errors.New("processor failed")

func (r *recorder) newProcessor() line.Processor {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.made++
	return &recordingProcessor{recorder: r, id: r.made}
}

func (r *recorder) add(e event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) snapshot() []event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event(nil), r.events...)
}

// lines returns the texts of the recorded lines.
func (r *recorder) lines() []string {
	var texts []string
	for _, e := range r.snapshot() {
		if e.kind == "line" {
			texts = append(texts, e.text)
		}
	}
	return texts
}

func (r *recorder) lineNums() []uint64 {
	var nums []uint64
	for _, e := range r.snapshot() {
		if e.kind == "line" {
			nums = append(nums, e.lineNum)
		}
	}
	return nums
}

func (r *recorder) hasLine(text string) bool {
	for _, got := range r.lines() {
		if got == text {
			return true
		}
	}
	return false
}

type recordingProcessor struct {
	recorder *recorder
	id       int
}

func (p *recordingProcessor) ProcessLine(buf *bytes.Buffer, lineNum uint64, source string) error {
	text := buf.String()
	pool.RecycleBytesBuffer(buf)
	p.recorder.add(event{kind: "line", text: text, lineNum: lineNum, source: source, id: p.id})
	if p.recorder.failAt != "" && text == p.recorder.failAt {
		return errProcessor
	}
	return nil
}

func (p *recordingProcessor) Flush() error {
	p.recorder.add(event{kind: "flush", id: p.id})
	return nil
}

func (p *recordingProcessor) Close() error {
	p.recorder.add(event{kind: "close", id: p.id})
	return nil
}

func (p *recordingProcessor) SourceRestarted() {
	p.recorder.add(event{kind: "restart", id: p.id})
}

// testFile is a log file that tests append to, rotate and truncate.
type testFile struct {
	t    *testing.T
	path string
}

func newTestFile(t *testing.T) *testFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.log")
	if err := os.WriteFile(path, []byte("existing line before any read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &testFile{t: t, path: path}
}

func (f *testFile) appendLines(lines ...string) {
	f.t.Helper()
	fd, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()
	for _, text := range lines {
		if _, err := fd.WriteString(text + "\n"); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *testFile) appendRaw(text string) {
	f.t.Helper()
	fd, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()
	if _, err := fd.WriteString(text); err != nil {
		f.t.Fatal(err)
	}
}

func (f *testFile) target() fs.ValidatedReadTarget {
	f.t.Helper()
	target, err := fs.NewValidatedReadTarget(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}

func newTestHub() *Hub {
	return New(Options{Logger: logging.NopLogger{}, RetryInterval: 10 * time.Millisecond})
}

// follower is one session following the test file through the hub.
type follower struct {
	recorder *recorder
	cancel   context.CancelFunc
	done     chan error
	messages chan string
	// failures counts the failed reads the hub reported to the session.
	failures atomic.Int64
}

func startFollower(t *testing.T, hub *Hub, file *testFile, ltx lcontext.LContext,
	re regex.Regex, globID string) *follower {

	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{
		recorder: &recorder{},
		cancel:   cancel,
		done:     make(chan error, 1),
		messages: make(chan string, 16),
	}
	session := Session{
		Target:         file.target(),
		FilePath:       file.path,
		GlobID:         globID,
		LContext:       ltx,
		Regex:          re,
		ServerMessages: f.messages,
		NewProcessor:   f.recorder.newProcessor,
		ReportFailure:  func() { f.failures.Add(1) },
	}
	go func() { f.done <- hub.Follow(ctx, session) }()
	t.Cleanup(func() {
		cancel()
		<-f.done
	})
	return f
}

// stop cancels the session and returns what Follow returned.
func (f *follower) stop(t *testing.T) error {
	t.Helper()
	f.cancel()
	select {
	case err := <-f.done:
		f.done <- err // for the cleanup
		return err
	case <-time.After(waitTimeout):
		t.Fatal("Follow did not return after its context ended")
		return nil
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	waitWithin(t, waitTimeout, what, condition)
}

// waitWithin is waitFor with a timeout of its own, for waits that include a
// whole burst read under the race detector on a loaded machine.
func waitWithin(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func subscriberCount(hub *Hub, path string) int {
	e := hub.entryFor(path)
	if e == nil {
		return 0
	}
	return len(e.snapshot())
}

// syncReader starts a session that sees every line and appends sync lines
// until it saw one, so the shared reader has the file open and has read all
// of it. Sessions that join afterwards get exactly the lines appended later.
func syncReader(t *testing.T, hub *Hub, file *testFile) *follower {
	t.Helper()
	syncer := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "sync")
	for i := 0; ; i++ {
		text := fmt.Sprintf("sync %d", i)
		file.appendLines(text)
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			if syncer.recorder.hasLine(text) {
				return syncer
			}
			time.Sleep(5 * time.Millisecond)
		}
		if i > 100 {
			t.Fatal("the shared reader never delivered a sync line")
		}
	}
}

// expectedFollow feeds lines, in follow form, to a fresh LineFilter: this is
// what a session joining before them gets from a private follow read.
func expectedFollow(ltx lcontext.LContext, re regex.Regex, lines []string) ([]string, []uint64) {
	want := &recorder{}
	filter := fs.NewLineFilter(ltx, want.newProcessor(), re, "glob")
	for _, text := range lines {
		if text == "" {
			continue // the follow reader skips empty lines
		}
		if stop, err := filter.ProcessLine([]byte(text)); stop || err != nil {
			break
		}
	}
	filter.Close()
	return want.lines(), want.lineNums()
}

func mustRegex(t *testing.T, pattern string) regex.Regex {
	t.Helper()
	re, err := regex.New(pattern, regex.Default)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

func TestFollowFansOutToEverySessionWithItsOwnFilter(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	syncReader(t, hub, file)

	sessions := []struct {
		name string
		ltx  lcontext.LContext
		re   regex.Regex
	}{
		{"all lines", lcontext.LContext{}, regex.NewNoop()},
		{"errors", lcontext.LContext{}, mustRegex(t, "ERROR")},
		{"errors with context", lcontext.LContext{BeforeContext: 1, AfterContext: 1}, mustRegex(t, "ERROR")},
	}
	followers := make([]*follower, len(sessions))
	for i, session := range sessions {
		followers[i] = startFollower(t, hub, file, session.ltx, session.re, "glob")
	}
	waitFor(t, "all sessions to join", func() bool { return subscriberCount(hub, file.path) == 4 })

	lines := []string{"INFO a", "ERROR b", "INFO c", "", "INFO d", "INFO e", "ERROR f", "INFO g"}
	file.appendLines(lines...)

	for i, session := range sessions {
		wantLines, wantNums := expectedFollow(session.ltx, session.re, lines)
		waitFor(t, session.name+" lines", func() bool { return len(followers[i].recorder.lines()) >= len(wantLines) })
		if got := followers[i].recorder.lines(); !reflect.DeepEqual(got, wantLines) {
			t.Errorf("%s: lines = %q, want %q", session.name, got, wantLines)
		}
		if got := followers[i].recorder.lineNums(); !reflect.DeepEqual(got, wantNums) {
			t.Errorf("%s: line numbers = %v, want %v", session.name, got, wantNums)
		}
		for _, e := range followers[i].recorder.snapshot() {
			if e.kind == "line" && e.source != "glob" {
				t.Errorf("%s: source ID = %q, want the session's glob ID", session.name, e.source)
			}
		}
	}
}

func TestFollowJoinMidStreamStartsAtNextLineWithLineOne(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	early := syncReader(t, hub, file)

	file.appendLines("one", "two", "three")
	waitFor(t, "early session to see three", func() bool { return early.recorder.hasLine("three") })

	late := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	waitFor(t, "late session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	file.appendLines("four", "five")
	waitFor(t, "late session to see five", func() bool { return late.recorder.hasLine("five") })

	if got, want := late.recorder.lines(), []string{"four", "five"}; !reflect.DeepEqual(got, want) {
		t.Errorf("late session lines = %q, want %q", got, want)
	}
	if got, want := late.recorder.lineNums(), []uint64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("late session line numbers = %v, want %v", got, want)
	}
	// The early session's numbering is not affected by the join.
	nums := early.recorder.lineNums()
	for i := 1; i < len(nums); i++ {
		if nums[i] != nums[i-1]+1 {
			t.Fatalf("early session line numbers are not consecutive: %v", nums)
		}
	}
}

func TestFollowPublishesTruncationInOrder(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	syncReader(t, hub, file)
	follower := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 2 })

	file.appendLines("old content line that is long enough to be truncated away")
	waitFor(t, "old line", func() bool { return len(follower.recorder.lines()) == 1 })

	// Rewrite in place with shorter content: the reader rewinds.
	if err := os.WriteFile(file.path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new line", func() bool { return follower.recorder.hasLine("new") })

	var kinds []string
	for _, e := range follower.recorder.snapshot() {
		if e.kind == "line" || e.kind == "restart" {
			kinds = append(kinds, e.kind+":"+e.text)
		}
	}
	want := []string{"line:old content line that is long enough to be truncated away", "restart:", "line:new"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("events = %q, want %q", kinds, want)
	}
	if got, want := follower.recorder.lineNums(), []uint64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("line numbers = %v, want %v (numbering carries on over a truncation)", got, want)
	}
}

func TestFollowReopensWithNewProcessorAfterRotation(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	syncReader(t, hub, file)
	follower := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 2 })

	file.appendLines("before rotation")
	waitFor(t, "line before rotation", func() bool { return follower.recorder.hasLine("before rotation") })

	if err := os.Rename(file.path, file.path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("first of new file\nsecond of new file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new file lines", func() bool { return follower.recorder.hasLine("second of new file") })

	// The first processor is flushed and closed before the new file's lines
	// go to a second processor, and the new file is read from its start.
	var sequence []string
	for _, e := range follower.recorder.snapshot() {
		switch e.kind {
		case "line":
			sequence = append(sequence, fmt.Sprintf("%d:%s:%d", e.id, e.text, e.lineNum))
		case "close":
			sequence = append(sequence, fmt.Sprintf("%d:close", e.id))
		}
	}
	want := []string{"1:before rotation:1", "1:close", "2:first of new file:2", "2:second of new file:3"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("sequence = %q, want %q", sequence, want)
	}
}

func TestFollowLastSessionLeavingStopsTheReader(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	first := syncReader(t, hub, file)
	second := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	waitFor(t, "second session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	e := hub.entryFor(file.path)

	if err := first.stop(t); err != nil {
		t.Fatalf("Follow() = %v, want nil after cancellation", err)
	}
	select {
	case <-e.done:
		t.Fatal("the reader stopped while a session was still following")
	default:
	}

	if err := second.stop(t); err != nil {
		t.Fatalf("Follow() = %v, want nil after cancellation", err)
	}
	select {
	case <-e.done:
	case <-time.After(waitTimeout):
		t.Fatal("the reader did not stop after the last session left")
	}
	if hub.entryFor(file.path) != nil {
		t.Error("the hub still lists the stopped read")
	}

	// Every processor a session made was flushed and closed.
	for _, f := range []*follower{first, second} {
		events := f.recorder.snapshot()
		if n := len(events); n < 2 || events[n-2].kind != "flush" || events[n-1].kind != "close" {
			t.Errorf("last events = %+v, want flush then close", events)
		}
	}

	// A new session starts a new reader.
	third := syncReader(t, hub, file)
	if hub.entryFor(file.path) == e {
		t.Error("a new session joined the stopped read")
	}
	_ = third
}

func TestFollowHandsTheTargetOverWhenItsSessionLeaves(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	var replaced []string
	replace := hub.seams.replaceTarget
	hub.seams.replaceTarget = func(reader *fs.ReadFile, target fs.ValidatedReadTarget) error {
		replaced = append(replaced, target.ResolvedPath())
		return replace(reader, target)
	}
	owner := syncReader(t, hub, file)
	other := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "other")
	waitFor(t, "second session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	e := hub.entryFor(file.path)

	if session, ok := e.ownerSession(); !ok || session.GlobID != "sync" {
		t.Fatalf("owner = %q, want the first session", session.GlobID)
	}
	if err := owner.stop(t); err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 1 || replaced[0] != file.path {
		t.Errorf("the reader was handed targets %q, want the remaining session's target for %s", replaced, file.path)
	}
	if session, ok := e.ownerSession(); !ok || session.GlobID != "other" {
		t.Fatalf("owner after leave = %q, %v, want the remaining session", session.GlobID, ok)
	}

	// Reading goes on, and a rotation reopens the file through the new owner.
	file.appendLines("after hand-over")
	waitFor(t, "line after hand-over", func() bool { return other.recorder.hasLine("after hand-over") })
	if err := os.Rename(file.path, file.path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("reopened\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "line after reopen", func() bool { return other.recorder.hasLine("reopened") })
}

func TestFollowReturnsWhenTheSessionReadEnds(t *testing.T) {
	tests := []struct {
		name    string
		ltx     lcontext.LContext
		failAt  string
		wantErr error
	}{
		{"max count", lcontext.LContext{MaxCount: 1}, "", ErrStopped},
		{"processor error", lcontext.LContext{}, "hit", errProcessor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newTestHub()
			file := newTestFile(t)
			syncer := syncReader(t, hub, file)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &recorder{failAt: tt.failAt}
			done := make(chan error, 1)
			go func() {
				done <- hub.Follow(ctx, Session{
					Target: file.target(), FilePath: file.path, GlobID: "glob",
					LContext: tt.ltx, Regex: mustRegex(t, "hit"), NewProcessor: rec.newProcessor,
				})
			}()
			waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
			file.appendLines("hit", "hit again")

			select {
			case err := <-done:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Follow() = %v, want %v", err, tt.wantErr)
				}
			case <-time.After(waitTimeout):
				t.Fatal("Follow did not return")
			}
			events := rec.snapshot()
			if n := len(events); n < 2 || events[n-2].kind != "flush" || events[n-1].kind != "close" {
				t.Errorf("last events = %+v, want flush then close", events)
			}
			// The other session keeps following.
			waitFor(t, "other session to see hit again", func() bool { return syncer.recorder.hasLine("hit again") })
			waitFor(t, "ended session to leave", func() bool { return subscriberCount(hub, file.path) == 1 })
		})
	}
}

// textLogger returns its arguments as the log line, like the real logger does,
// so that the reader's warning messages carry their text.
type textLogger struct{ logging.NopLogger }

func (textLogger) Warn(args ...any) string { return fmt.Sprint(args...) }

func TestFollowWarnsEverySessionAboutLongLinesWithItsOwnPath(t *testing.T) {
	hub := New(Options{Logger: textLogger{}, RetryInterval: 10 * time.Millisecond, MaxLineLength: 16})
	file := newTestFile(t)
	syncReader(t, hub, file)

	// A second name for the same file: both sessions share one reader.
	link := filepath.Join(t.TempDir(), "link.log")
	if err := os.Symlink(file.path, link); err != nil {
		t.Fatal(err)
	}
	direct := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	viaLink := startFollowerWithPath(t, hub, file, link)
	waitFor(t, "sessions to join", func() bool { return subscriberCount(hub, file.path) == 3 })

	// A follow reader only splits, and warns about, a line it has not seen
	// the end of yet, so write the long line without its newline first.
	file.appendRaw(strings.Repeat("x", 40))
	for _, tt := range []struct {
		f    *follower
		path string
	}{{direct, file.path}, {viaLink, link}} {
		select {
		case message := <-tt.f.messages:
			if !strings.Contains(message, "Long log line") || !strings.Contains(message, tt.path) {
				t.Errorf("message = %q, want the long line warning for %s", message, tt.path)
			}
		case <-time.After(waitTimeout):
			t.Fatal("a session did not get the long line warning")
		}
	}
}

// startFollowerWithPath follows the test file under another name.
func startFollowerWithPath(t *testing.T, hub *Hub, file *testFile, path string) *follower {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{recorder: &recorder{}, cancel: cancel, done: make(chan error, 1), messages: make(chan string, 16)}
	session := Session{
		Target: file.target(), FilePath: path, GlobID: "link", Regex: regex.NewNoop(),
		ServerMessages: f.messages, Logger: textLogger{}, NewProcessor: f.recorder.newProcessor,
	}
	go func() { f.done <- hub.Follow(ctx, session) }()
	t.Cleanup(func() {
		cancel()
		<-f.done
	})
	return f
}

func TestFollowFlushesAfterEveryPublishedRead(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	syncReader(t, hub, file)
	follower := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 2 })

	file.appendLines("a", "b")
	waitFor(t, "both lines", func() bool { return follower.recorder.hasLine("b") })
	waitFor(t, "the flush after them", func() bool {
		events := follower.recorder.snapshot()
		return len(events) > 0 && events[len(events)-1].kind == "flush"
	})
	events := follower.recorder.snapshot()
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.kind)
	}
	if want := []string{"line", "line", "flush"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("events = %q, want %q (one flush after the read's lines)", kinds, want)
	}
}

func TestFollowSurvivesAReaderPanic(t *testing.T) {
	tests := []struct {
		name  string
		start func(context.Context, *fs.ReadFile, line.Processor) error
	}{
		{"panic", func(context.Context, *fs.ReadFile, line.Processor) error { panic("injected reader bug") }},
		{"worker panic error", func(context.Context, *fs.ReadFile, line.Processor) error {
			return fmt.Errorf("%w: injected", fs.ErrReaderWorkerPanic)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newTestHub()
			file := newTestFile(t)
			healthy := hub.seams.startReader
			release := make(chan struct{})
			hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
				<-release // let both sessions join first
				return tt.start(ctx, reader, processor)
			}

			followers := []*follower{
				startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "a"),
				startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "b"),
			}
			waitFor(t, "sessions to join", func() bool { return subscriberCount(hub, file.path) == 2 })
			failed := hub.entryFor(file.path)
			close(release)

			for _, f := range followers {
				select {
				case err := <-f.done:
					f.done <- err
					if !errors.Is(err, ErrReaderFailed) || !errors.Is(err, fs.ErrReaderWorkerPanic) {
						t.Errorf("Follow() = %v, want an error wrapping ErrReaderFailed and fs.ErrReaderWorkerPanic", err)
					}
				case <-time.After(waitTimeout):
					t.Fatal("Follow did not return after the reader failed")
				}
			}
			select {
			case <-failed.done:
			case <-time.After(waitTimeout):
				t.Fatal("the failed reader did not stop")
			}

			// The hub forgot the failed read: a new session gets a new reader.
			hub.seams.startReader = healthy
			syncReader(t, hub, file)
			if hub.entryFor(file.path) == failed {
				t.Error("a new session joined the failed read")
			}
		})
	}
}
func TestFollowRejectsInvalidSessions(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	valid := Session{Target: file.target(), FilePath: file.path, NewProcessor: (&recorder{}).newProcessor}
	journal, err := fs.NewValidatedJournalTarget("journal:dtail.service")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Session)
	}{
		{"journal target", func(s *Session) { s.Target = journal }},
		{"no target", func(s *Session) { s.Target = fs.ValidatedReadTarget{} }},
		{"no path", func(s *Session) { s.FilePath = "" }},
		{"no processor factory", func(s *Session) { s.NewProcessor = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := valid
			tt.mutate(&session)
			if err := hub.Follow(context.Background(), session); err == nil {
				t.Fatal("Follow() accepted an invalid session")
			}
			if hub.entryFor(file.path) != nil {
				t.Fatal("an invalid session started a shared read")
			}
		})
	}
}

// TestFollowMatchesPrivateFollowRead runs a private follow reader and a shared
// session side by side on the same file and compares what they deliver after
// both have seen a common sync line.
func TestFollowMatchesPrivateFollowRead(t *testing.T) {
	contexts := []lcontext.LContext{{}, {BeforeContext: 2}, {AfterContext: 1}, {BeforeContext: 1, AfterContext: 2}}
	for _, ltx := range contexts {
		t.Run(fmt.Sprintf("%+v", ltx), func(t *testing.T) {
			hub := newTestHub()
			file := newTestFile(t)
			re := mustRegex(t, "ERROR|sync")

			shared := startFollower(t, hub, file, ltx, re, "glob")
			private := &recorder{}
			ctx, cancel := context.WithCancel(context.Background())
			privateDone := make(chan error, 1)
			reader, err := fs.NewReadFile(fs.ReadOptions{
				Mode: omode.TailClient, FilePath: file.path, GlobID: "glob", SeekEOF: true,
				Logger: logging.NopLogger{},
			})
			if err != nil {
				t.Fatal(err)
			}
			go func() { privateDone <- reader.Start(ctx, ltx, private.newProcessor(), re) }()
			defer func() {
				cancel()
				<-privateDone
			}()

			// Append sync lines until both readers have seen the same one.
			var marker string
			for i := 0; marker == ""; i++ {
				text := fmt.Sprintf("sync %d", i)
				file.appendLines(text)
				time.Sleep(50 * time.Millisecond)
				if shared.recorder.hasLine(text) && private.hasLine(text) {
					marker = text
				}
				if i > 200 {
					t.Fatal("the readers never saw a common sync line")
				}
			}

			lines := []string{"INFO 1", "ERROR 2", "INFO 3", "INFO 4", "INFO 5", "ERROR 6", "", "INFO 7", "ERROR 8", "last"}
			file.appendLines(lines...)
			file.appendLines("sync end")
			waitFor(t, "both readers to finish", func() bool {
				return shared.recorder.hasLine("sync end") && private.hasLine("sync end")
			})

			gotLines, gotSteps := afterMarker(shared.recorder, marker)
			wantLines, wantSteps := afterMarker(private, marker)
			if !reflect.DeepEqual(gotLines, wantLines) {
				t.Errorf("shared lines = %q, private lines = %q", gotLines, wantLines)
			}
			// Absolute numbers differ by how many sync lines each reader saw
			// before the marker; the numbering from the marker on must match.
			if !reflect.DeepEqual(gotSteps, wantSteps) {
				t.Errorf("shared line number steps = %v, private = %v", gotSteps, wantSteps)
			}
		})
	}
}

// afterMarker returns the lines after the marker line and their line numbers
// relative to the marker's.
func afterMarker(r *recorder, marker string) ([]string, []uint64) {
	var lines []string
	var steps []uint64
	var base uint64
	seen := false
	for _, e := range r.snapshot() {
		if e.kind != "line" {
			continue
		}
		if !seen {
			if e.text == marker {
				seen, base = true, e.lineNum
			}
			continue
		}
		lines = append(lines, e.text)
		steps = append(steps, e.lineNum-base)
	}
	return lines, steps
}

func TestFailedReadIsForgottenWhileSessionsAreStillSubscribed(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	release := make(chan struct{})
	healthy := hub.seams.startReader
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return healthy(ctx, reader, processor)
	}
	defer close(release)

	// A subscriber that is registered but not draining its queue yet, as a
	// session busy with earlier lines would be.
	sub := newSubscriber(Session{Target: file.target(), FilePath: file.path,
		NewProcessor: (&recorder{}).newProcessor}, 4)
	e := hub.join(sub)
	defer hub.leave(e, sub)

	failure := fmt.Errorf("%w: injected", fs.ErrReaderWorkerPanic)
	e.fail(failure)
	e.fail(errors.New("a second failure must be ignored"))

	if hub.entryFor(file.path) != nil {
		t.Error("the hub still lists the failed read while a session is subscribed")
	}
	select {
	case it := <-sub.queue:
		if it.kind != failedItem || !errors.Is(it.err, failure) {
			t.Errorf("queued item = %+v, want the failure", it)
		}
	default:
		t.Fatal("the subscriber was not told about the failure")
	}
	select {
	case it := <-sub.queue:
		t.Errorf("a second failure was queued: %+v", it)
	default:
	}
}

func TestFailedHandOverMovesTheRemainingSessionToAPrivateRead(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	hub.seams.replaceTarget = func(*fs.ReadFile, fs.ValidatedReadTarget) error {
		return errors.New("injected hand-over failure")
	}
	owner := syncReader(t, hub, file)
	other := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "other")
	waitFor(t, "second session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	file.appendLines("before the failure")
	waitFor(t, "line before the failure", func() bool { return other.recorder.hasLine("before the failure") })
	failed := hub.entryFor(file.path)

	if err := owner.stop(t); err != nil {
		t.Fatal(err)
	}
	select {
	case <-failed.done:
	case <-time.After(waitTimeout):
		t.Fatal("the reader did not stop after the hand-over failed")
	}
	// The failure is not the session's: it goes on privately, from where the
	// shared read left it, without losing or repeating a line.
	file.appendLines("after the failure 1", "after the failure 2")
	waitFor(t, "lines after the failure", func() bool { return other.recorder.hasLine("after the failure 2") })
	select {
	case err := <-other.done:
		t.Fatalf("Follow() = %v after a failed hand-over, want the session to go on", err)
	default:
	}
	if got, want := other.recorder.lines(), []string{"before the failure", "after the failure 1",
		"after the failure 2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if got, want := other.recorder.lineNums(), []uint64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("line numbers = %v, want %v", got, want)
	}
}
