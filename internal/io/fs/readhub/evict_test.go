package readhub

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

// capturingLogger keeps the Info and Warn lines it is given.
type capturingLogger struct {
	logging.NopLogger
	mu    sync.Mutex
	lines []string
}

func (l *capturingLogger) Info(args ...any) string { return l.add("INFO", args) }
func (l *capturingLogger) Warn(args ...any) string { return l.add("WARN", args) }

func (l *capturingLogger) add(level string, args []any) string {
	text := level + " " + fmt.Sprintln(args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, text)
	return text
}

// count returns how many captured lines contain every one of parts.
func (l *capturingLogger) count(parts ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, text := range l.lines {
		matches := true
		for _, part := range parts {
			matches = matches && strings.Contains(text, part)
		}
		if matches {
			n++
		}
	}
	return n
}

// gatedProcessor processes a line only once its gate is open, like the
// processor of a session whose client stopped reading.
type gatedProcessor struct {
	line.Processor
	gate <-chan struct{}
	// entered, if set, is signalled when a line waits for the gate.
	entered chan<- struct{}
}

func (p gatedProcessor) ProcessLine(buf *bytes.Buffer, lineNum uint64, source string) error {
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	<-p.gate
	return p.Processor.ProcessLine(buf, lineNum, source)
}

// lastLineProcessor keeps only the last line and the line count, so that a
// session using it keeps up with the shared reader even under the race
// detector while a test polls it.
type lastLineProcessor struct {
	mu    sync.Mutex
	last  string
	count int
}

func (p *lastLineProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = buf.String()
	p.count++
	return nil
}

func (p *lastLineProcessor) Flush() error { return nil }
func (p *lastLineProcessor) Close() error { return nil }

func (p *lastLineProcessor) state() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last, p.count
}

// startFastFollower starts a session that sees every line and keeps up.
func startFastFollower(t *testing.T, hub *Hub, file *testFile) *lastLineProcessor {
	t.Helper()
	processor := &lastLineProcessor{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	session := Session{
		Target: file.target(), FilePath: file.path, GlobID: "fast", Regex: regex.NewNoop(),
		NewProcessor: func() line.Processor { return processor },
	}
	go func() { done <- hub.Follow(ctx, session) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return processor
}

func (p *lastLineProcessor) saw(text string) bool {
	last, _ := p.state()
	return last == text
}

// startGatedFollower starts a session whose processors wait for gate, and
// signal entered, if given, when they do. The cleanup opens the gate, should
// the test have failed before it did.
func startGatedFollower(t *testing.T, hub *Hub, file *testFile, ltx lcontext.LContext,
	re regex.Regex, gate chan struct{}, entered ...chan struct{}) *follower {

	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{recorder: &recorder{}, cancel: cancel, done: make(chan error, 1)}
	session := Session{
		Target: file.target(), FilePath: file.path, GlobID: "slow", LContext: ltx, Regex: re,
		NewProcessor: func() line.Processor {
			processor := gatedProcessor{Processor: f.recorder.newProcessor(), gate: gate}
			if len(entered) > 0 {
				processor.entered = entered[0]
			}
			return processor
		},
	}
	go func() { f.done <- hub.Follow(ctx, session) }()
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		cancel()
		<-f.done
	})
	return f
}

// appendPaced appends lines in batches of about two chunks, a quarter of the
// queue of newEvictingHub, and waits for fast to get each batch before it
// writes the next, so that only a stuck session falls behind by more than a
// few chunks. The follow reader polls a file it read to its end every 100 ms,
// so the fewer batches, the faster the test.
func appendPaced(t *testing.T, file *testFile, fast *lastLineProcessor, lines []string) {
	t.Helper()
	const batch = 1500
	for start := 0; start < len(lines); start += batch {
		end := min(start+batch, len(lines))
		// One write per batch, so that the reader reads it in few reads.
		file.appendRaw(strings.Join(lines[start:end], "\n") + "\n")
		waitFor(t, "fast session to get a batch", func() bool { return fast.saw(lines[end-1]) })
	}
}

// burstLines is how many lines a burst has: many more chunks than a
// session's queue holds.
const burstLines = 20000

// burst returns burstLines log lines, every seventh an ERROR, long enough
// that they fill many chunks.
func burst(prefix string) []string {
	lines := make([]string, burstLines)
	for i := range lines {
		level := "INFO"
		if i%7 == 3 {
			level = "ERROR"
		}
		lines[i] = fmt.Sprintf("%s %s %05d %s", level, prefix, i, strings.Repeat("x", 60))
	}
	return lines
}

func newEvictingHub(logger logging.Logger) *Hub {
	return New(Options{Logger: logger, RetryInterval: 10 * time.Millisecond, QueueChunks: 8})
}

func TestEvictedSessionLosesNoLineAndKeepsNumberingAndContext(t *testing.T) {
	contexts := []lcontext.LContext{{}, {BeforeContext: 2}, {AfterContext: 3}, {BeforeContext: 1, AfterContext: 2}}
	for _, ltx := range contexts {
		t.Run(fmt.Sprintf("%+v", ltx), func(t *testing.T) {
			// Each case has a hub and file of its own.
			t.Parallel()
			logger := &capturingLogger{}
			hub := newEvictingHub(logger)
			file := newTestFile(t)
			fast := startFastFollower(t, hub, file)
			waitFor(t, "fast session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
			gate := make(chan struct{})
			re := mustRegex(t, "ERROR|tail")
			slow := startGatedFollower(t, hub, file, ltx, re, gate)
			waitFor(t, "slow session to join", func() bool { return subscriberCount(hub, file.path) == 2 })

			// The stuck session does not hold up the other one.
			lines := burst("burst")
			appendPaced(t, file, fast, lines)
			waitFor(t, "slow session to be evicted", func() bool { return subscriberCount(hub, file.path) == 1 })
			if logger.count("INFO", "evicted a slow subscriber", "subscribers=1") != 1 {
				t.Errorf("no INFO eviction line with the subscriber count in %q", logger.lines)
			}

			// Lines appended after the eviction reach the session privately.
			tail := []string{"tail 1", "INFO filler", "tail 2", "INFO filler 2", "INFO filler 3"}
			file.appendLines(tail...)
			close(gate)
			all := append(append([]string(nil), lines...), tail...)
			wantLines, wantNums := expectedFollow(ltx, re, all)
			last := wantLines[len(wantLines)-1]
			waitFor(t, "slow session to catch up", func() bool { return slow.recorder.hasLine(last) })
			// Let a wrong extra line, should there be one, arrive too.
			time.Sleep(100 * time.Millisecond)

			if got := slow.recorder.lines(); !reflect.DeepEqual(got, wantLines) {
				t.Errorf("slow session got %d lines, want %d; first difference %s",
					len(got), len(wantLines), firstDifference(got, wantLines))
			}
			if got := slow.recorder.lineNums(); !reflect.DeepEqual(got, wantNums) {
				t.Errorf("slow session line numbers differ from one uninterrupted read: %s",
					firstDifference(got, wantNums))
			}
		})
	}
}

// firstDifference describes where got and want first differ.
func firstDifference[T comparable](got, want []T) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("at %d: got %v, want %v", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("lengths %d and %d", len(got), len(want))
}

func TestEvictingTheLastSessionStopsTheSharedRead(t *testing.T) {
	logger := &capturingLogger{}
	hub := newEvictingHub(logger)
	file := newTestFile(t)
	gate := make(chan struct{})
	slow := startGatedFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), gate)
	waitFor(t, "session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	shared := hub.entryFor(file.path)

	lines := burst("only")
	file.appendLines(lines...)
	select {
	case <-shared.done:
	case <-time.After(waitTimeout):
		t.Fatal("the shared read did not stop after its last session was evicted")
	}
	if hub.entryFor(file.path) != nil {
		t.Error("the hub still lists a shared read without sessions")
	}
	if n := logger.count("Shared follow read stopped"); n != 1 {
		t.Errorf("logged the stop %d times, want once", n)
	}

	close(gate)
	waitFor(t, "session to read everything", func() bool { return slow.recorder.hasLine(lines[len(lines)-1]) })
	// Having caught up privately, the session starts a new shared read.
	waitFor(t, "session to rejoin", func() bool { return subscriberCount(hub, file.path) == 1 })
	if n := logger.count("INFO", "rejoined", "subscribers=1"); n != 1 {
		t.Errorf("logged the rejoin %d times, want once: %q", n, logger.lines)
	}
	file.appendLines("after rejoin")
	lines = append(lines, "after rejoin")
	waitFor(t, "session to get a line after its rejoin", func() bool { return slow.recorder.hasLine("after rejoin") })
	wantLines, wantNums := expectedFollow(lcontext.LContext{}, regex.NewNoop(), lines)
	if got := slow.recorder.lines(); !reflect.DeepEqual(got, wantLines) {
		t.Errorf("session lines differ: %s", firstDifference(got, wantLines))
	}
	if got := slow.recorder.lineNums(); !reflect.DeepEqual(got, wantNums) {
		t.Errorf("session line numbers differ: %s", firstDifference(got, wantNums))
	}
	if err := slow.stop(t); err != nil {
		t.Errorf("Follow() = %v, want nil", err)
	}
	if n := logger.count("Shared follow read stopped"); n != 2 {
		t.Errorf("logged the stop %d times, want twice: after the eviction and after the rejoined session left", n)
	}
}

func TestEvictedSessionReadsTheOldFileToItsEndAfterARotation(t *testing.T) {
	logger := &capturingLogger{}
	hub := newEvictingHub(logger)
	file := newTestFile(t)
	fast := startFastFollower(t, hub, file)
	waitFor(t, "fast session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	gate := make(chan struct{})
	slow := startGatedFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), gate)
	waitFor(t, "slow session to join", func() bool { return subscriberCount(hub, file.path) == 2 })

	lines := burst("old")
	appendPaced(t, file, fast, lines)
	waitFor(t, "slow session to be evicted", func() bool { return subscriberCount(hub, file.path) == 1 })

	if err := os.Rename(file.path, file.path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("new 1\nnew 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fast session to read the new file", func() bool { return fast.saw("new 2") })
	close(gate)
	waitFor(t, "slow session to read the new file", func() bool { return slow.recorder.hasLine("new 2") })

	// The slow session got the whole old file, as its private reader holds
	// the file opened at the eviction, then, like a private reader after a
	// rotation, the whole new file with a new processor. No line was lost.
	got := slow.recorder.snapshot()
	var oldLines, newLines []string
	var nums []uint64
	for _, e := range got {
		if e.kind != "line" {
			continue
		}
		nums = append(nums, e.lineNum)
		if strings.HasPrefix(e.text, "new") {
			newLines = append(newLines, e.text)
			if e.id < 2 {
				t.Errorf("new file line %q went to the old file's processor", e.text)
			}
			continue
		}
		oldLines = append(oldLines, e.text)
	}
	if !reflect.DeepEqual(oldLines, lines) {
		t.Errorf("old file lines differ from the burst (%d of %d lines): %s",
			len(oldLines), len(lines), firstDifference(oldLines, lines))
	}
	if want := []string{"new 1", "new 2"}; !reflect.DeepEqual(newLines, want) {
		t.Errorf("new file lines = %q, want %q", newLines, want)
	}
	for i := 1; i < len(nums); i++ {
		if nums[i] != nums[i-1]+1 {
			t.Fatalf("line numbers are not consecutive at %d: %d then %d", i, nums[i-1], nums[i])
		}
	}
	if n := logger.count("WARN", "rotated"); n != 0 {
		t.Errorf("warned %d times about lines lost to the rotation, none were: %q", n, logger.lines)
	}
}

func TestLateJoinerStartsAtTheEndOfTheFile(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	release := make(chan struct{})
	healthy := hub.seams.startReader
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		select {
		case <-release:
		case <-ctx.Done():
			return nil
		}
		return healthy(ctx, reader, processor)
	}

	// The shared reader lags: the first session's reader has not read
	// anything yet when the backlog is written and the second session joins.
	first := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "first")
	waitFor(t, "first session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	file.appendLines("backlog 1", "backlog 2")
	late := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "late")
	waitFor(t, "late session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	file.appendLines("after join")
	close(release)

	waitFor(t, "both sessions to see the line after the join", func() bool {
		return first.recorder.hasLine("after join") && late.recorder.hasLine("after join")
	})
	// Each session starts at the end of the file as of its join, like a
	// private follow read opened then.
	if got, want := first.recorder.lines(), []string{"backlog 1", "backlog 2", "after join"}; !reflect.DeepEqual(got, want) {
		t.Errorf("first session lines = %q, want %q", got, want)
	}
	if got, want := late.recorder.lines(), []string{"after join"}; !reflect.DeepEqual(got, want) {
		t.Errorf("late session lines = %q, want %q", got, want)
	}
	if got, want := late.recorder.lineNums(), []uint64{1}; !reflect.DeepEqual(got, want) {
		t.Errorf("late session line numbers = %v, want %v", got, want)
	}
}

func TestFanoutPublishesTheLongLineWarningBetweenItsLines(t *testing.T) {
	fanout, sub := fanoutUnderTest(t)
	messages := fanout.entry.(*entry).messages
	if err := fanout.ProcessRawLine([]byte("before"), 0, ""); err != nil {
		t.Fatal(err)
	}
	// The reader warns, then feeds the split line.
	messages <- "warning"
	if err := fanout.ProcessRawLine([]byte("split"), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := fanout.Flush(); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, it := range drain(sub) {
		switch it.kind {
		case chunkItem:
			got = append(got, strings.Join(chunkLines(it.chunk), ","))
		case longLineItem:
			got = append(got, "warning")
		}
	}
	if want := []string{"before", "warning", "split"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}
}

func TestFollowRejectsCompressedFiles(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	for _, path := range []string{file.path + ".gz", file.path + ".zst"} {
		session := Session{Target: file.target(), FilePath: path, NewProcessor: (&recorder{}).newProcessor}
		if err := hub.Follow(context.Background(), session); err == nil {
			t.Errorf("Follow() accepted the compressed file %s", path)
		}
	}
}
