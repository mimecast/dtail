package readhub

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
)

// eofGate holds a follow reader once, when armed, after a read that returned
// nothing at end, the end of the file: after the reader's last read of the
// file and before it checks whether the path was rotated.
type eofGate struct {
	armed   atomic.Bool
	end     atomic.Int64
	last    atomic.Int64
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newEOFGate() *eofGate {
	g := &eofGate{reached: make(chan struct{}), release: make(chan struct{})}
	g.last.Store(-1)
	return g
}

// arm makes the gate hold its reader at its next empty read at end.
func (g *eofGate) arm(end int64) {
	g.end.Store(end)
	g.armed.Store(true)
}

// readUpTo waits for release, the first time the reader's read returned
// nothing at the end of the file after arming.
func (g *eofGate) readUpTo(offset int64) {
	previous := g.last.Swap(offset)
	if offset == previous && offset == g.end.Load() && g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		<-g.release
	}
}

func (g *eofGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *eofGate) awaitHeld(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(3 * waitTimeout):
		t.Fatalf("timed out waiting for %s to reach the end of the file", what)
	}
}

// eofGatedFanout is the shared reader's processor, held by gate.
type eofGatedFanout struct {
	*fanoutProcessor
	gate *eofGate
}

func (g eofGatedFanout) ReadUpTo(offset int64, file os.FileInfo) {
	g.gate.readUpTo(offset)
	g.fanoutProcessor.ReadUpTo(offset, file)
}

// eofGatedProcessor is a private reader's processor, held by gate. It
// observes positions only for that.
type eofGatedProcessor struct {
	line.Processor
	gate *eofGate
}

func (eofGatedProcessor) ReadStarting()                          {}
func (g eofGatedProcessor) ReadUpTo(offset int64, _ os.FileInfo) { g.gate.readUpTo(offset) }
func (eofGatedProcessor) LineEndsAt(int64, os.FileInfo)          {}

// SourceRestarted passes the restart on, which the embedded interface hides.
func (g eofGatedProcessor) SourceRestarted() {
	if restarter, ok := g.Processor.(line.SourceRestarter); ok {
		restarter.SourceRestarted()
	}
}

// Lines appended to a file right before it is rotated, after the readers'
// last read of it and before they find the rotation, and lines a writer that
// has not reopened the path appends to the old file after the rename, are
// read from the old file before the new one, by the shared reader and a
// private reader alike, so that shared sessions get what a private read does.
func TestSharedAndPrivateReadsDrainTheRotatedFile(t *testing.T) {
	tests := []struct {
		name         string
		beforeRename []string
		afterRename  []string
	}{
		{name: "appended before the rename", beforeRename: []string{"ERROR old 1", "tail old"}},
		{name: "appended to the old file after the rename", afterRename: []string{"ERROR late 1", "tail late"}},
		{name: "both", beforeRename: []string{"tail old"}, afterRename: []string{"tail late"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testSharedAndPrivateReadsDrainTheRotatedFile(t, tt.beforeRename, tt.afterRename)
		})
	}
}

func testSharedAndPrivateReadsDrainTheRotatedFile(t *testing.T, beforeRename, afterRename []string) {
	hub := newTestHub()
	sharedGate := newEOFGate()
	healthy := hub.seams.startReader
	hub.seams.startReader = func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
		gated := eofGatedFanout{fanoutProcessor: processor.(*fanoutProcessor), gate: sharedGate}
		return healthy(ctx, reader, gated)
	}
	file := newTestFile(t)
	re := mustRegex(t, "ERROR|new|tail")
	start, err := endOfFile(file.target())
	if err != nil {
		t.Fatal(err)
	}
	privateGate, privateContextGate := newEOFGate(), newEOFGate()
	private := wrappedPrivateFollower(t, file, lcontext.LContext{}, re, start, func(p line.Processor) line.Processor {
		return eofGatedProcessor{Processor: p, gate: privateGate}
	})
	t.Cleanup(privateGate.open)
	privateContext := wrappedPrivateFollower(t, file, lcontext.LContext{BeforeContext: 1}, re, start,
		func(p line.Processor) line.Processor {
			return eofGatedProcessor{Processor: p, gate: privateContextGate}
		})
	t.Cleanup(privateContextGate.open)
	first := startFollower(t, hub, file, lcontext.LContext{}, re, "first")
	waitFor(t, "first session to join", func() bool { return subscriberCount(hub, file.path) == 1 })
	second := startFollower(t, hub, file, lcontext.LContext{BeforeContext: 1}, re, "second")
	waitFor(t, "second session to join", func() bool { return subscriberCount(hub, file.path) == 2 })
	t.Cleanup(sharedGate.open)

	file.appendLines("INFO a", "ERROR b", "tail c")
	readers := []*recorder{first.recorder, second.recorder, private, privateContext}
	for _, rec := range readers {
		waitFor(t, "the lines before the rotation", func() bool { return rec.hasLine("tail c") })
	}
	info, err := os.Stat(file.path)
	if err != nil {
		t.Fatal(err)
	}
	gates := map[string]*eofGate{"the shared reader": sharedGate, "the private read": privateGate,
		"the private read with context": privateContextGate}
	for _, gate := range gates {
		gate.arm(info.Size())
	}
	for what, gate := range gates {
		gate.awaitHeld(t, what)
	}

	// The old file's writer, which has not reopened the path.
	writer, err := os.OpenFile(file.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	writeLines(t, writer, beforeRename)
	if err := os.Rename(file.path, file.path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.path, []byte("new 1\nINFO new 2\nnew 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLines(t, writer, afterRename)
	for _, gate := range gates {
		gate.open()
	}
	for _, rec := range readers {
		waitFor(t, "the new file", func() bool { return rec.hasLine("new 3") })
	}
	time.Sleep(100 * time.Millisecond)

	for _, text := range append(append([]string(nil), beforeRename...), afterRename...) {
		for name, rec := range map[string]*recorder{"first": first.recorder, "private": private} {
			if !rec.hasLine(text) {
				t.Errorf("the %s read did not get %q from the rotated file", name, text)
			}
		}
	}
	assertSameStream(t, first.recorder, private)
	assertSameStream(t, second.recorder, privateContext)
}

func writeLines(t *testing.T, writer *os.File, lines []string) {
	t.Helper()
	for _, text := range lines {
		if _, err := writer.WriteString(text + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}
