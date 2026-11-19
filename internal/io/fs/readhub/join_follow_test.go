package readhub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

// numbered returns count lines "prefix 1" to "prefix count".
func numbered(prefix string, count int) []string {
	lines := make([]string, count)
	for i := range lines {
		lines[i] = fmt.Sprintf("%s %d", prefix, i+1)
	}
	return lines
}

// processorIDs returns the IDs of the processors the lines went to.
func processorIDs(r *recorder) []int {
	var ids []int
	for _, e := range r.snapshot() {
		if e.kind == "line" {
			ids = append(ids, e.id)
		}
	}
	return ids
}

func (r *recorder) processors() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.made
}

func countEvents(r *recorder, kind string) int {
	n := 0
	for _, e := range r.snapshot() {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// A session that joins after the path was rotated, while the shared reader
// still has the old file, starts at the end of the new file, like a private
// follow read opened then, not at its beginning like the shared reader.
func TestJoinAfterARotationStartsAtTheEndOfTheNewFile(t *testing.T) {
	for _, when := range []string{"before the reader moved on", "before the reader opened the new file"} {
		t.Run(when, func(t *testing.T) {
			hub := newTestHub()
			file := newTestFile(t)
			healthy := hub.seams.startReader
			paused, release := make(chan struct{}), make(chan struct{})
			var starts atomic.Int32
			hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
				n := starts.Add(1)
				if n == 2 && when == "before the reader opened the new file" {
					// The reader announced the new read but has not opened
					// the file yet.
					close(paused)
					<-release
				}
				err := healthy(ctx, reader, processor)
				if n == 1 && when == "before the reader moved on" {
					// The reader found the rotation; it has not announced
					// the new read yet.
					close(paused)
					<-release
				}
				return err
			}
			first := syncReader(t, hub, file)
			file.appendLines("old 1")
			waitFor(t, "the old file's line", func() bool { return first.recorder.hasLine("old 1") })

			if err := os.Rename(file.path, file.path+".1"); err != nil {
				t.Fatal(err)
			}
			pre := numbered("pre", 100)
			if err := os.WriteFile(file.path, []byte(strings.Join(pre, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			<-paused
			late := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "late")
			waitFor(t, "late session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
			close(release)
			waitFor(t, "the first session to read the new file", func() bool { return first.recorder.hasLine("pre 100") })
			// A copytruncate after the join: a private read opened at the
			// join reads the rewritten file from its beginning, too.
			if err := os.WriteFile(file.path, []byte("post 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "both sessions to get the line after the join", func() bool {
				return first.recorder.hasLine("post 1") && late.recorder.hasLine("post 1")
			})

			// The late session gets what a private read opened at its join
			// gets, with one processor; the first one reads the new file
			// from its beginning with a new processor.
			if got, want := late.recorder.lines(), []string{"post 1"}; !reflect.DeepEqual(got, want) {
				t.Errorf("late session lines = %d lines %q..., want %q", len(got), got[:min(3, len(got))], want)
			}
			if got, want := late.recorder.lineNums(), []uint64{1}; !reflect.DeepEqual(got, want) {
				t.Errorf("late session line numbers = %v, want %v", got, want)
			}
			if made := late.recorder.processors(); made != 1 {
				t.Errorf("late session made %d processors, want 1", made)
			}
			lines := first.recorder.lines()
			if want := append(append([]string{"old 1"}, pre...), "post 1"); !reflect.DeepEqual(lines[len(lines)-len(want):], want) {
				t.Errorf("first session did not read the old line, the whole new file and the new line: %q", lines)
			}
		})
	}
}

// gatedFanout holds the shared reader in Flush, before it publishes what it
// read, while armed.
type gatedFanout struct {
	*fanoutProcessor
	armed   *atomic.Bool
	blocked chan struct{}
	release chan struct{}
}

func (g gatedFanout) Flush() error {
	if g.armed.CompareAndSwap(true, false) {
		close(g.blocked)
		<-g.release
	}
	return g.fanoutProcessor.Flush()
}

// A session that joins after the file was truncated and rewritten in place,
// before the shared reader noticed, skips the old content the reader still
// publishes and the rewritten content up to its join.
func TestJoinAfterACopytruncateStartsAtTheEndOfTheRewrittenFile(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	healthy := hub.seams.startReader
	var armed atomic.Bool
	blocked, release := make(chan struct{}), make(chan struct{})
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		gated := gatedFanout{fanoutProcessor: processor.(*fanoutProcessor), armed: &armed, blocked: blocked, release: release}
		return healthy(ctx, reader, gated)
	}
	first := syncReader(t, hub, file)

	// The reader read the old content but has not published it yet.
	armed.Store(true)
	old := numbered("old", 50)
	file.appendLines(old...)
	<-blocked
	rewritten := numbered("new", 5)
	if err := os.WriteFile(file.path, []byte(strings.Join(rewritten, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	late := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "late")
	waitFor(t, "late session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	close(release)
	waitFor(t, "the first session to read the rewritten file", func() bool { return first.recorder.hasLine("new 5") })
	file.appendLines("after join")
	waitFor(t, "both sessions to get the line after the join", func() bool {
		return first.recorder.hasLine("after join") && late.recorder.hasLine("after join")
	})

	if got, want := late.recorder.lines(), []string{"after join"}; !reflect.DeepEqual(got, want) {
		t.Errorf("late session lines = %q, want %q", got, want)
	}
	if n := countEvents(late.recorder, "restart"); n != 0 {
		t.Errorf("late session's processor was told %d times that the file started over, want 0", n)
	}
	lines := first.recorder.lines()
	want := append(append(append([]string(nil), old...), rewritten...), "after join")
	if !reflect.DeepEqual(lines[len(lines)-len(want):], want) {
		t.Errorf("first session did not read the old content, the rewritten file and the new line: %q", lines)
	}
	if n := countEvents(first.recorder, "restart"); n != 1 {
		t.Errorf("first session's processor was told %d times that the file started over, want 1", n)
	}
}

// restartGatedFanout holds the shared reader when it reports its first read
// after it started the file over, before it feeds that read's lines.
type restartGatedFanout struct {
	*fanoutProcessor
	restarted *atomic.Bool
	blocked   chan struct{}
	release   chan struct{}
}

func (g restartGatedFanout) SourceRestarted() {
	g.fanoutProcessor.SourceRestarted()
	g.restarted.Store(true)
}

func (g restartGatedFanout) ReadUpTo(offset int64, file os.FileInfo) {
	if g.restarted.CompareAndSwap(true, false) {
		close(g.blocked)
		<-g.release
	}
	g.fanoutProcessor.ReadUpTo(offset, file)
}

// A session that joins after the shared reader started a truncated file over,
// before it read on, is not mistaken for one that joined before the reader
// noticed the truncation: it gets the lines appended after its join.
func TestJoinRightAfterATruncationGetsTheLinesAfterIt(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	healthy := hub.seams.startReader
	var restarted atomic.Bool
	blocked, release := make(chan struct{}), make(chan struct{})
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		gated := restartGatedFanout{fanoutProcessor: processor.(*fanoutProcessor), restarted: &restarted,
			blocked: blocked, release: release}
		return healthy(ctx, reader, gated)
	}
	first := syncReader(t, hub, file)
	file.appendLines(numbered("old", 50)...)
	waitFor(t, "the old content", func() bool { return first.recorder.hasLine("old 50") })

	if err := os.WriteFile(file.path, []byte("new 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-blocked
	late := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "late")
	waitFor(t, "late session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	file.appendLines("after join")
	close(release)
	waitFor(t, "both sessions to get the line after the join", func() bool {
		return first.recorder.hasLine("after join") && late.recorder.hasLine("after join")
	})
	if got, want := late.recorder.lines(), []string{"after join"}; !reflect.DeepEqual(got, want) {
		t.Errorf("late session lines = %q, want %q", got, want)
	}
}

// A session that joins while the last subscriber of the file's shared read is
// being evicted, before that read stops, still gets the lines appended after
// it joined.
func TestJoinWhileTheLastSessionIsEvicted(t *testing.T) {
	hub := newEvictingHub(&capturingLogger{})
	file := newTestFile(t)
	var joiner *lastLineProcessor
	joined := make(chan struct{})
	hub.seams.evicted = func(remaining int) {
		if remaining != 0 || joiner != nil {
			return
		}
		// Another session joins in the window between the eviction of the
		// last subscriber and the stop of the shared read.
		joiner = startFastFollower(t, hub, file)
		deadline := time.Now().Add(2 * time.Second)
		for subscriberCount(hub, file.path) != 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		close(joined)
	}
	gate := make(chan struct{})
	startGatedFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), gate)
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 1 })

	// One write, so that the file holds all of it when the session joins.
	file.appendRaw(strings.Join(burst("only"), "\n") + "\n")
	select {
	case <-joined:
	case <-time.After(waitTimeout):
		t.Fatal("the only session was not evicted")
	}
	waitFor(t, "the joiner's shared read", func() bool { return subscriberCount(hub, file.path) == 1 })
	file.appendLines("after join 1", "after join 2")
	waitFor(t, "the joiner to get the lines appended after it joined", func() bool { return joiner.saw("after join 2") })
	if _, count := joiner.state(); count != 2 {
		t.Errorf("the joiner got %d lines, want the 2 appended after it joined", count)
	}
}

// A session evicted with a truncation or rotation that did not fit into its
// queue still handles it, as the private read it goes on with would not see
// it: here a rotation, which needs no warning about lost lines, because the
// shared reader had read the old file to its end.
func TestEvictedSessionHandlesTheRotationThatDidNotFit(t *testing.T) {
	logger := &capturingLogger{}
	hub := New(Options{Logger: logger, RetryInterval: 10 * time.Millisecond, QueueChunks: 1})
	file := newTestFile(t)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	slow := startGatedFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), gate, entered)
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	sub := hub.entryFor(file.path).snapshot()[0]

	// The session takes the first line and waits in its processor; the
	// second fills its queue.
	file.appendLines("a")
	<-entered
	file.appendLines("b")
	waitFor(t, "the second line to be queued", func() bool { return len(sub.queue) == 1 })
	if err := os.Rename(file.path, file.path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("new 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the eviction", func() bool { return logger.count("evicted a slow subscriber") == 1 })
	close(gate)
	waitFor(t, "the new file", func() bool { return slow.recorder.hasLine("new 1") })

	if got, want := slow.recorder.lines(), []string{"a", "b", "new 1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if got, want := processorIDs(slow.recorder), []int{1, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("processors = %v, want %v (the new file with a new one)", got, want)
	}
	if n := logger.count("WARN", "rotated"); n != 0 {
		t.Errorf("warned %d times about lines lost to the rotation, none were: %q", n, logger.lines)
	}
}

// A session evicted by the failure of a panicking shared reader, which did
// not fit into its queue, ends its read with the panic like every other
// session instead of going on privately.
func TestEvictedSessionGetsTheReaderPanicThatDidNotFit(t *testing.T) {
	logger := &capturingLogger{}
	hub := New(Options{Logger: logger, RetryInterval: 10 * time.Millisecond, QueueChunks: 1})
	file := newTestFile(t)
	published := make(chan struct{})
	hub.seams.startReader = func(ctx context.Context, _ *fs.ReadFile, processor line.Processor) error {
		fanout := processor.(*fanoutProcessor)
		for _, text := range []string{"a", "b"} {
			if err := fanout.ProcessRawLine([]byte(text), 0, ""); err != nil {
				return err
			}
			if err := fanout.Flush(); err != nil {
				return err
			}
			<-published
		}
		panic("injected reader bug")
	}
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	slow := startGatedFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), gate, entered)
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	sub := hub.entryFor(file.path).snapshot()[0]
	// "a" is taken, "b" fills the queue, the failure does not fit.
	<-entered
	published <- struct{}{}
	waitFor(t, "the second line to be queued", func() bool { return len(sub.queue) == 1 })
	published <- struct{}{}
	waitFor(t, "the eviction", func() bool { return logger.count("evicted a slow subscriber") == 1 })
	close(gate)

	select {
	case err := <-slow.done:
		slow.done <- err
		if !errors.Is(err, ErrReaderFailed) || !errors.Is(err, fs.ErrReaderWorkerPanic) {
			t.Errorf("Follow() = %v, want an error wrapping ErrReaderFailed and fs.ErrReaderWorkerPanic", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Follow did not return after the shared reader panicked")
	}
	if got, want := slow.recorder.lines(), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

// messageSession is a session of path whose messages the test reads.
func messageSession(t *testing.T, path string, rec *recorder) (Session, chan string) {
	t.Helper()
	target, err := fs.NewValidatedReadTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	messages := make(chan string, 16)
	return Session{
		Target: target, FilePath: path, GlobID: "glob", Regex: regex.NewNoop(),
		ServerMessages: messages, Logger: textLogger{}, NewProcessor: rec.newProcessor,
	}, messages
}

// A long line warning that precedes a line the session skips because it
// predates the join is not sent: a private read opened at the join does not
// read that line.
func TestSkippedLongLineIsNotWarnedAbout(t *testing.T) {
	file := newTestFile(t)
	info, err := os.Stat(file.path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	session, messages := messageSession(t, file.path, rec)
	joinedAt := position{offset: 100, file: info}
	read := newSessionRead(session, logging.NopLogger{}, Options{}, joinedAt, newJoinSkip(joinedAt, joinedAt))
	defer read.close()
	chunkEndingAt := func(text string, end int64) item {
		return item{kind: chunkItem, chunk: &chunk{data: []byte(text), ends: []int{len(text)}, offsets: []int64{end}, file: info}}
	}

	ctx := context.Background()
	for _, it := range []item{
		{kind: longLineItem}, chunkEndingAt("before the join", 90),
		chunkEndingAt("after the join", 150),
		{kind: longLineItem}, chunkEndingAt("split after the join", 250),
	} {
		if _, err := read.handle(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := rec.lines(), []string{"after the join", "split after the join"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if n := len(messages); n != 1 {
		t.Errorf("sent %d long line warnings, want 1, for the split line after the join", n)
	}
}

// readPrivatelyUntil runs read's private follow read until the recorder has
// line want.
func readPrivatelyUntil(t *testing.T, read *sessionRead, rec *recorder, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- read.readPrivately(ctx, nil) }()
	waitFor(t, "line "+want, func() bool { return rec.hasLine(want) })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("readPrivately() = %v", err)
	}
}

// A session that goes on privately where the shared reader split a long line
// does not warn about the rest of it again, like one private read.
func TestPrivateReadInASplitLineDoesNotWarnAgain(t *testing.T) {
	const maxLineLength = 8
	for _, atLineEnd := range []bool{true, false} {
		t.Run(fmt.Sprintf("atLineEnd=%v", atLineEnd), func(t *testing.T) {
			file := newTestFile(t)
			if err := os.WriteFile(file.path, []byte(strings.Repeat("a", maxLineLength)+strings.Repeat("b", 2*maxLineLength)), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(file.path)
			if err != nil {
				t.Fatal(err)
			}
			rec := &recorder{}
			session, messages := messageSession(t, file.path, rec)
			at := position{offset: maxLineLength, file: info}
			read := newSessionRead(session, logging.NopLogger{}, Options{MaxLineLength: maxLineLength}, at, joinSkip{})
			defer read.close()
			// The shared reader fed the split line's first part, or the
			// session joined there.
			read.atLineEnd = atLineEnd

			readPrivatelyUntil(t, read, rec, strings.Repeat("b", 2*maxLineLength))
			want := 0
			if !atLineEnd {
				want = 1
			}
			if n := len(messages); n != want {
				t.Errorf("sent %d long line warnings, want %d", n, want)
			}
		})
	}
}

// A session whose private read finds the path rotated since the last line it
// handled reads the new file from its beginning with a new processor, as a
// private read after a rotation does, and warns about the lines it may miss.
func TestPrivateReadOfARotatedFileUsesANewProcessor(t *testing.T) {
	file := newTestFile(t)
	info, err := os.Stat(file.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file.path, file.path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("new 1\nnew 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	session, _ := messageSession(t, file.path, rec)
	logger := &capturingLogger{}
	at := position{offset: info.Size(), file: info}
	read := newSessionRead(session, logger, Options{RetryInterval: 10 * time.Millisecond}, at, joinSkip{})
	defer read.close()
	read.atLineEnd = true

	readPrivatelyUntil(t, read, rec, "new 2")
	if got, want := rec.lines(), []string{"new 1", "new 2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if got, want := processorIDs(rec), []int{2, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("processors = %v, want %v", got, want)
	}
	if n := logger.count("WARN", "rotated"); n != 1 {
		t.Errorf("warned %d times about the rotation, want once", n)
	}
}

// An entry whose last subscriber was evicted takes no new subscriber, even
// one that found it in the hub before, so the session starts a new entry
// instead of joining a read that stops.
func TestClosedEntryTakesNoSubscriber(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	session := Session{Target: file.target(), FilePath: file.path, Regex: regex.NewNoop(),
		NewProcessor: (&recorder{}).newProcessor}
	first := newSubscriber(session, 1)
	e := hub.join(first)
	t.Cleanup(func() { hub.leave(e, first) })

	e.publishMu.Lock()
	e.evict(first, item{kind: chunkItem})
	e.publishMu.Unlock()
	if e.join(newSubscriber(session, 1), unknownPosition) {
		t.Fatal("a closed entry took a new subscriber")
	}
	// The hub drops the entry when it closes; a session that looks it up
	// just before must not join it either.
	hub.mu.Lock()
	hub.entries[e.key] = e
	hub.mu.Unlock()
	next := newSubscriber(session, 1)
	if joined := hub.join(next); joined == e {
		t.Error("the hub joined a session to a closed entry")
	} else {
		hub.leave(joined, next)
	}
}
