package readhub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// openedFiles records the subscriber descriptors a hub opens.
type openedFiles struct {
	mu    sync.Mutex
	files []*os.File
}

func recordOpenedFiles(hub *Hub) *openedFiles {
	opened := &openedFiles{}
	open := hub.seams.openFile
	hub.seams.openFile = func(target fs.ValidatedReadTarget) (*os.File, error) {
		fd, err := open(target)
		if err == nil {
			opened.mu.Lock()
			opened.files = append(opened.files, fd)
			opened.mu.Unlock()
		}
		return fd, err
	}
	return opened
}

// open returns how many of the recorded descriptors are still open.
func (o *openedFiles) open() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, fd := range o.files {
		if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
			n++
		}
	}
	return n
}

// privateFollower follows a file like the read command does without a shared
// reader: one reader, started again with a new processor after every read,
// e.g. after a rotation. It starts at start, the end of the file when the
// shared sessions it is compared with joined.
func privateFollower(t *testing.T, file *testFile, ltx lcontext.LContext, re regex.Regex,
	start position) *recorder {

	t.Helper()
	return wrappedPrivateFollower(t, file, ltx, re, start, nil)
}

// wrappedPrivateFollower is privateFollower with each processor passed
// through wrap, unless it is nil.
func wrappedPrivateFollower(t *testing.T, file *testFile, ltx lcontext.LContext, re regex.Regex,
	start position, wrap func(line.Processor) line.Processor) *recorder {

	t.Helper()
	target := file.target()
	options := fs.ReadOptions{
		Mode: omode.TailClient, Target: &target, FilePath: file.path, GlobID: "private",
		Logger: logging.NopLogger{},
	}
	start.startAt(&options)
	reader, err := fs.NewReadFile(options)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			processor := rec.newProcessor()
			if wrap != nil {
				processor = wrap(processor)
			}
			_ = reader.Start(ctx, ltx, processor, re)
			_ = processor.Flush()
			_ = processor.Close()
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return rec
}

// sessionSubscriber returns the subscriber of the session with globID.
func sessionSubscriber(t *testing.T, hub *Hub, file *testFile, globID string) *subscriber {
	t.Helper()
	for _, sub := range hub.entryFor(file.path).snapshot() {
		if sub.session.GlobID == globID {
			return sub
		}
	}
	t.Fatalf("no subscriber %s", globID)
	return nil
}

// lineEvents returns the recorded lines with their numbers and processors.
func lineEvents(r *recorder) []string {
	var lines []string
	for _, e := range r.snapshot() {
		if e.kind == "line" {
			lines = append(lines, fmt.Sprintf("%d:%d:%s", e.id, e.lineNum, e.text))
		}
	}
	return lines
}

// A size-triggered logrotate right after a burst larger than a session's
// queue: the path is rotated while the shared reader is still behind in the
// old file, and the session is evicted only afterwards. It still reads the
// rest of the old file, through the descriptor it holds of the file the
// reader has open, and then the new file, exactly like a private read.
func TestEvictionAfterARotationWhileTheReaderLagsLosesNoLine(t *testing.T) {
	contexts := []lcontext.LContext{{}, {BeforeContext: 1, AfterContext: 2}}
	for _, ltx := range contexts {
		t.Run(fmt.Sprintf("%+v", ltx), func(t *testing.T) {
			logger := &capturingLogger{}
			hub := newEvictingHub(logger)
			opened := recordOpenedFiles(hub)
			armed := &atomic.Bool{}
			paused, release := make(chan struct{}), make(chan struct{})
			healthy := hub.seams.startReader
			hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
				gated := gatedFanout{fanoutProcessor: processor.(*fanoutProcessor), armed: armed,
					blocked: paused, release: release}
				return healthy(ctx, reader, gated)
			}
			file := newTestFile(t)
			re := mustRegex(t, "ERROR|new")
			start, err := endOfFile(file.target())
			if err != nil {
				t.Fatal(err)
			}
			private := privateFollower(t, file, ltx, re, start)
			fast := startFastFollower(t, hub, file)
			waitFor(t, "fast session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
			gate := make(chan struct{})
			slow := startGatedFollower(t, hub, file, ltx, re, gate)
			waitFor(t, "slow session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
			slowSub := sessionSubscriber(t, hub, file, "slow")

			// The shared reader stops after its first read of the burst,
			// long before the session's queue is full, while the path is
			// rotated.
			armed.Store(true)
			old := burst("old")
			file.appendRaw(strings.Join(old, "\n") + "\n")
			<-paused
			if err := os.Rename(file.path, file.path+".1"); err != nil {
				t.Fatal(err)
			}
			newLines := []string{"new 1", "INFO new 2", "new 3"}
			if err := os.WriteFile(file.path, []byte(strings.Join(newLines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			close(release)
			select {
			case <-slowSub.evicted:
			case <-time.After(waitTimeout):
				t.Fatal("the slow session was not evicted")
			}
			waitFor(t, "fast session to read the new file", func() bool { return fast.saw("new 3") })
			close(gate)
			waitFor(t, "slow session to read the new file", func() bool { return slow.recorder.hasLine("new 3") })
			waitFor(t, "private read to read the new file", func() bool { return private.hasLine("new 3") })
			// Let a wrong extra line, should there be one, arrive too.
			time.Sleep(100 * time.Millisecond)

			got, want := lineEvents(slow.recorder), lineEvents(private)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("evicted session got %d lines, a private read %d; first difference %s",
					len(got), len(want), firstDifference(got, want))
			}
			if n := logger.count("WARN", "rotated"); n != 0 {
				t.Errorf("warned %d times about lines lost to the rotation, none were: %q", n, logger.lines)
			}

			beforeStop := opened.open()
			if err := slow.stop(t); err != nil {
				t.Errorf("Follow() = %v, want nil", err)
			}
			// A rejoin also opens the new reader's start descriptor through the
			// seam; it can remain open alongside the subscriber descriptors.
			if n, subscribers := opened.open(), subscriberCount(hub, file.path); n >= beforeStop || n > subscribers+1 {
				t.Errorf("%d descriptors remain after stopping the slow session (before %d, subscribers %d)",
					n, beforeStop, subscribers)
			}
		})
	}
}

// A session evicted after its read ended, before it left the shared read,
// does not leak the descriptor it holds.
func TestEvictionAfterTheReadEndedClosesTheHeldFile(t *testing.T) {
	logger := &capturingLogger{}
	hub := newEvictingHub(logger)
	opened := recordOpenedFiles(hub)
	file := newTestFile(t)
	fast := startFastFollower(t, hub, file)
	waitFor(t, "fast session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	leaving := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "leaving")
	waitFor(t, "second session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	appendPaced(t, file, fast, []string{"synced"})
	waitFor(t, "both descriptors", func() bool { return opened.open() == 2 })

	// The session's read ends, but the hub's lock keeps it from leaving the
	// shared read until it is evicted.
	hub.mu.Lock()
	locked := true
	defer func() {
		if locked {
			hub.mu.Unlock()
		}
	}()
	leaving.cancel()
	time.Sleep(50 * time.Millisecond)
	appendPaced(t, file, fast, burst("evict"))
	waitFor(t, "the eviction", func() bool { return logger.count("evicted a slow subscriber") == 1 })
	hub.mu.Unlock()
	locked = false
	if err := leaving.stop(t); err != nil {
		t.Fatalf("Follow() = %v, want nil", err)
	}

	if n := opened.open(); n != 1 {
		t.Errorf("%d subscriber descriptors are open after the session left, want only the fast one's", n)
	}
}

// Every descriptor a shared read opened for its subscribers is closed once
// every session left, including those of rotated-away files.
func TestSubscriberDescriptorsAreClosedAfterRotations(t *testing.T) {
	hub := newTestHub()
	opened := recordOpenedFiles(hub)
	file := newTestFile(t)
	first := syncReader(t, hub, file)
	second := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "second")
	waitFor(t, "second session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	for i := range 3 {
		if err := os.Rename(file.path, fmt.Sprintf("%s.%d", file.path, i)); err != nil {
			t.Fatal(err)
		}
		text := fmt.Sprintf("file %d", i)
		if err := os.WriteFile(file.path, []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, text, func() bool { return first.recorder.hasLine(text) && second.recorder.hasLine(text) })
		if n := opened.open(); n != 2 {
			t.Errorf("after rotation %d, %d subscriber descriptors are open, want one per session", i, n)
		}
	}
	shared := hub.entryFor(file.path)
	for _, f := range []*follower{first, second} {
		if err := f.stop(t); err != nil {
			t.Fatalf("Follow() = %v, want nil", err)
		}
	}
	<-shared.done
	if n := opened.open(); n != 0 {
		t.Errorf("%d subscriber descriptors are still open after every session left", n)
	}
}

// A descriptor set after the held file was taken over or closed is closed at
// once: nobody would close it otherwise.
func TestHeldFileClosesADescriptorSetAfterItEnded(t *testing.T) {
	file := newTestFile(t)
	for _, end := range []string{"take", "close"} {
		t.Run(end, func(t *testing.T) {
			var held heldFile
			first, err := os.Open(file.path)
			if err != nil {
				t.Fatal(err)
			}
			held.set(first)
			switch end {
			case "take":
				if got := held.take(); got != first {
					t.Fatalf("take() = %v, want the held descriptor", got)
				}
				closeHeld(first)
			case "close":
				held.close()
				if _, statErr := first.Stat(); !errors.Is(statErr, os.ErrClosed) {
					t.Errorf("close() left the held descriptor open")
				}
			}
			late, err := os.Open(file.path)
			if err != nil {
				t.Fatal(err)
			}
			held.set(late)
			if _, err := late.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Error("a descriptor set after the end was left open")
				closeHeld(late)
			}
			if got := held.take(); got != nil {
				t.Errorf("take() after the end = %v, want nil", got)
			}
		})
	}
}

// holds decides whether the private reader goes on in the held file: only if
// it is the file of the session's position, or the session is at the start of
// the read the reader opened it for.
func TestSessionReadHoldsOnlyTheFileOfItsPosition(t *testing.T) {
	file := newTestFile(t)
	held, err := os.Open(file.path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHeld(held)
	heldInfo, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	other := newTestFile(t)
	otherInfo, err := os.Stat(other.path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		at   position
		want bool
	}{
		{"in the held file", position{offset: 5, file: heldInfo}, true},
		{"in another file", position{offset: 5, file: otherInfo}, false},
		{"start of a new read", position{offset: 0}, true},
		{"start of the held file", position{offset: 0, file: heldInfo}, true},
		{"start of another file", position{offset: 0, file: otherInfo}, false},
		{"unknown", unknownPosition(), false},
	}
	for _, tt := range tests {
		read := &sessionRead{at: tt.at}
		if got := read.holds(held); got != tt.want {
			t.Errorf("%s: holds() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
