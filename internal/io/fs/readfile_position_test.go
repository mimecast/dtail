package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// positionProcessor records lines and the line ends a follow reader reports.
type positionProcessor struct {
	mu      sync.Mutex
	lines   []string
	offsets []int64
	files   []os.FileInfo
	// readUpTo and readFiles record the ReadUpTo calls.
	readUpTo  []int64
	readFiles []os.FileInfo
	// calls records the ReadStarting ("starting") and ReadUpTo ("up to")
	// calls in order.
	calls []string
}

func (p *positionProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, buf.String())
	pool.RecycleBytesBuffer(buf)
	return nil
}

func (p *positionProcessor) Flush() error { return nil }
func (p *positionProcessor) Close() error { return nil }

func (p *positionProcessor) LineEndsAt(offset int64, file os.FileInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offsets = append(p.offsets, offset)
	p.files = append(p.files, file)
}

func (p *positionProcessor) ReadStarting() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "starting")
}

func (p *positionProcessor) ReadUpTo(offset int64, file os.FileInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "up to")
	p.readUpTo = append(p.readUpTo, offset)
	p.readFiles = append(p.readFiles, file)
}

func (p *positionProcessor) state() ([]string, []int64, []os.FileInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...), append([]int64(nil), p.offsets...),
		append([]os.FileInfo(nil), p.files...)
}

func appendToFile(t *testing.T, path, text string) {
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

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFollowReaderReportsWhereEachLineEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "positions.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const maxLineLength = 16
	reader := newFollowReadFile(path, "glob", nil, maxLineLength, testLogger)
	processor := &positionProcessor{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop()) }()
	defer func() {
		cancel()
		<-done
	}()

	// The reader seeks to the end first; append until it reads what follows.
	waitUntil(t, "the reader to pick up a line", func() bool {
		appendToFile(t, path, "probe\n")
		time.Sleep(20 * time.Millisecond)
		lines, _, _ := processor.state()
		return len(lines) > 0
	})
	// Empty lines are skipped but still take their byte, an unfinished line
	// is not fed, and a line split at the maximum length ends at the split.
	appendToFile(t, path, "a\n\n\nbb\ncomplete\nunfinish")
	waitUntil(t, "the complete line", func() bool {
		lines, _, _ := processor.state()
		return len(lines) > 0 && lines[len(lines)-1] == "complete"
	})
	appendToFile(t, path, "ed and too long for one line")
	waitUntil(t, "the split line", func() bool {
		lines, _, _ := processor.state()
		return len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "unfinish")
	})
	appendToFile(t, path, "\nlast\n")
	waitUntil(t, "the last line", func() bool {
		lines, _, _ := processor.state()
		return len(lines) > 0 && lines[len(lines)-1] == "last"
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	lines, offsets, files := processor.state()
	if len(offsets) != len(lines) {
		t.Fatalf("got %d offsets for %d lines", len(offsets), len(lines))
	}
	splitEnd := int64(strings.Index(content, "long for one line") + len("long for one line"))
	want := map[string]int64{
		"a":        int64(strings.Index(content, "a\n") + 2),
		"bb":       int64(strings.Index(content, "bb\n") + 3),
		"complete": int64(strings.Index(content, "complete\n") + 9),
		"last":     int64(len(content)),
	}
	checked := 0
	for i, text := range lines {
		wantOffset, ok := want[text]
		if strings.HasPrefix(text, "unfinish") {
			wantOffset, ok = splitEnd, true
		}
		if !ok {
			continue
		}
		checked++
		if offsets[i] != wantOffset {
			t.Errorf("line %q ends at %d, want %d", text, offsets[i], wantOffset)
		}
	}
	if checked != len(want)+1 {
		t.Errorf("checked %d lines, want %d: %q", checked, len(want)+1, lines)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(files[len(files)-1], info) {
		t.Error("the reported file identity is not the file being read")
	}

	// The reader reports where it starts, at the end of the file it seeked
	// to, where the first line it fed starts, and after its last read how far
	// it has read the file.
	processor.mu.Lock()
	readUpTo, readFiles, calls := processor.readUpTo, processor.readFiles, processor.calls
	processor.mu.Unlock()
	// Every read is announced before, and its position reported after it,
	// whether or not it returned data; the last read may still be running.
	if len(calls) < 2 || calls[0] != "up to" {
		t.Fatalf("reader made the calls %v, want ReadUpTo first", calls)
	}
	for i := 1; i < len(calls); i++ {
		want := "starting"
		if i%2 == 0 {
			want = "up to"
		}
		if calls[i] != want {
			t.Fatalf("call %d of %d is %q, want %q: reads and their positions do not alternate",
				i, len(calls), calls[i], want)
		}
	}
	firstLineStart := offsets[0] - int64(len(lines[0])) - 1
	if len(readUpTo) < 2 || readUpTo[0] != firstLineStart || readUpTo[len(readUpTo)-1] != int64(len(content)) {
		t.Errorf("read up to %v, want %d first and %d last", readUpTo, firstLineStart, len(content))
	}
	for i := 1; i < len(readUpTo); i++ {
		if readUpTo[i] < readUpTo[i-1] {
			t.Errorf("read offsets went back: %v", readUpTo)
		}
	}
	if len(readFiles) > 0 && !os.SameFile(readFiles[len(readFiles)-1], info) {
		t.Error("the file identity reported with the read offset is not the file being read")
	}
}

func TestReadersWithoutAnObserverOrForCompressedFilesReportNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()

	plain := newFollowReadFile(path, "glob", nil, defaultMaxLineLength, testLogger)
	if reporter, err := plain.newPositionReporter(fd, nil, &captureProcessor{}); reporter != nil || err != nil {
		t.Errorf("reporter for a processor without LineEndsAt = %v, %v, want nil", reporter, err)
	}
	gz := newFollowReadFile(path+".gz", "glob", nil, defaultMaxLineLength, testLogger)
	if reporter, err := gz.newPositionReporter(fd, nil, &positionProcessor{}); reporter != nil || err != nil {
		t.Errorf("reporter for a compressed file = %v, %v, want nil", reporter, err)
	}
	if reporter, err := plain.newPositionReporter(nil, nil, &positionProcessor{}); reporter != nil || err != nil {
		t.Errorf("reporter for the stdin pipe = %v, %v, want nil", reporter, err)
	}
	var none *positionReporter
	none.readStarting()
	if err := none.readReturned(1); err != nil {
		t.Errorf("a nil reporter reported an error: %v", err)
	}
	none.lineEndsAt(1)
}

func TestReplaceTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.log")
	other := filepath.Join(dir, "other.log")
	for _, p := range []string{path, other} {
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target, err := NewValidatedReadTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	sameFile, err := NewValidatedReadTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	otherTarget, err := NewValidatedReadTarget(other)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewValidatedJournalTarget("journal:dtail.service")
	if err != nil {
		t.Fatal(err)
	}

	withTarget := mustNewReadFile(ReadOptions{Mode: omode.TailClient, Target: &target, FilePath: path})
	if err := withTarget.ReplaceTarget(sameFile); err != nil {
		t.Errorf("replacing with a target for the same file: %v", err)
	}
	if withTarget.validatedTarget.Load().ResolvedPath() != path {
		t.Error("the replaced target does not name the file")
	}
	if err := withTarget.ReplaceTarget(otherTarget); err == nil {
		t.Error("a target for another file was accepted")
	}
	if err := withTarget.ReplaceTarget(journal); err == nil {
		t.Error("a journal target was accepted")
	}
	withoutTarget := mustNewReadFile(ReadOptions{Mode: omode.TailClient, FilePath: path})
	if err := withoutTarget.ReplaceTarget(target); err == nil {
		t.Error("a reader without a target was given one")
	}
}
