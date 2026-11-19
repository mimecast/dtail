package fs

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// rawCaptureProcessor implements both line.Processor and line.RawProcessor and
// records which of the two entry points delivered each line. Borrowed raw
// slices are copied before returning, as the RawProcessor contract requires.
type rawCaptureProcessor struct {
	lines       []string
	lineNums    []uint64
	sourceIDs   []string
	rawCalls    int
	bufferCalls int
	rawErr      error
	// lastRaw keeps the most recent borrowed slice header only so tests can
	// check that the fast path hands over the caller's slice without copying.
	// It is never read for content after the call.
	lastRaw []byte
}

var (
	_ line.Processor    = (*rawCaptureProcessor)(nil)
	_ line.RawProcessor = (*rawCaptureProcessor)(nil)
)

func (p *rawCaptureProcessor) ProcessLine(buf *bytes.Buffer, lineNum uint64, sourceID string) error {
	p.bufferCalls++
	p.record(buf.Bytes(), lineNum, sourceID)
	pool.RecycleBytesBuffer(buf)
	return nil
}

func (p *rawCaptureProcessor) ProcessRawLine(raw []byte, lineNum uint64, sourceID string) error {
	p.rawCalls++
	p.lastRaw = raw
	p.record(raw, lineNum, sourceID)
	return p.rawErr
}

func (p *rawCaptureProcessor) Flush() error { return nil }
func (p *rawCaptureProcessor) Close() error { return nil }

func (p *rawCaptureProcessor) record(content []byte, lineNum uint64, sourceID string) {
	p.lines = append(p.lines, string(content))
	p.lineNums = append(p.lineNums, lineNum)
	p.sourceIDs = append(p.sourceIDs, sourceID)
}

// rawTailCaptureProcessor is the follow-mode counterpart: lines are delivered
// on a channel because the tail runs in its own goroutine.
type rawTailCaptureProcessor struct {
	tailCaptureProcessor
	rawCalls    atomic.Int64
	bufferCalls atomic.Int64
}

func (p *rawTailCaptureProcessor) ProcessLine(buf *bytes.Buffer, lineNum uint64, sourceID string) error {
	p.bufferCalls.Add(1)
	return p.tailCaptureProcessor.ProcessLine(buf, lineNum, sourceID)
}

func (p *rawTailCaptureProcessor) ProcessRawLine(raw []byte, _ uint64, _ string) error {
	p.rawCalls.Add(1)
	p.lines <- string(raw)
	return nil
}

// retainingProcessor lacks the RawProcessor fast path and keeps the buffers it
// is handed, so a test can inspect them after the call.
type retainingProcessor struct {
	bufs []*bytes.Buffer
}

func (p *retainingProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	p.bufs = append(p.bufs, buf)
	return nil
}

func (p *retainingProcessor) Flush() error { return nil }
func (p *retainingProcessor) Close() error { return nil }

func mustRegex(t *testing.T, pattern string) regex.Regex {
	t.Helper()
	if pattern == "" {
		return regex.NewNoop()
	}
	re, err := regex.New(pattern, regex.Default)
	if err != nil {
		t.Fatalf("build regex %q: %v", pattern, err)
	}
	return re
}

// TestStartUsesRawProcessorFastPath checks that a snapshot read without local
// context delivers every emitted line through ProcessRawLine, never through
// ProcessLine, and that lines, line numbers and source IDs are identical to
// what the buffer path delivers to a processor without the fast path.
func TestStartUsesRawProcessorFastPath(t *testing.T) {
	const content = "apple 1\nbanana ERROR\napricot\ncherry ERROR 2\n\navocado ERROR"

	tests := []struct {
		name    string
		pattern string
	}{
		{name: "cat noop", pattern: ""},
		{name: "grep literal", pattern: "ERROR"},
		{name: "grep regex", pattern: "^a[a-z]+"},
		{name: "grep no match", pattern: "NEVER_MATCHES"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := writeProcessorTestFile(t, content)

			reference := &captureProcessor{}
			refReader := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
			if err := refReader.Start(context.Background(), lcontext.LContext{}, reference, mustRegex(t, tt.pattern)); err != nil {
				t.Fatalf("reference read: %v", err)
			}

			processor := &rawCaptureProcessor{}
			reader := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
			if err := reader.Start(context.Background(), lcontext.LContext{}, processor, mustRegex(t, tt.pattern)); err != nil {
				t.Fatalf("raw read: %v", err)
			}

			if processor.bufferCalls != 0 {
				t.Fatalf("ProcessLine called %d times, want 0 on the raw fast path", processor.bufferCalls)
			}
			if processor.rawCalls != len(reference.lines) {
				t.Fatalf("ProcessRawLine called %d times, want %d", processor.rawCalls, len(reference.lines))
			}
			if !equalStrings(processor.lines, reference.lines) {
				t.Fatalf("lines = %q, want %q", processor.lines, reference.lines)
			}
			if !equalUint64s(processor.lineNums, reference.lineNums) {
				t.Fatalf("line numbers = %v, want %v", processor.lineNums, reference.lineNums)
			}
			for i, sourceID := range processor.sourceIDs {
				if sourceID != "glob-id" {
					t.Fatalf("sourceID[%d] = %q, want glob-id", i, sourceID)
				}
			}
		})
	}
}

// Before-context retains lines; max/after-only reads can borrow them. Both
// delivery paths must produce the same payload and numbering.
func TestStartLocalContextSelectsRawProcessor(t *testing.T) {
	const content = "a\nb\nHIT 1\nc\nd\nHIT 2\ne\nHIT 3\nf\n"

	tests := []struct {
		name string
		ltx  lcontext.LContext
	}{
		{name: "before", ltx: lcontext.LContext{BeforeContext: 1}},
		{name: "after", ltx: lcontext.LContext{AfterContext: 1}},
		{name: "max", ltx: lcontext.LContext{MaxCount: 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := writeProcessorTestFile(t, content)

			reference := &captureProcessor{}
			refReader := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
			if err := refReader.Start(context.Background(), tt.ltx, reference, mustRegex(t, "HIT")); err != nil {
				t.Fatalf("reference read: %v", err)
			}

			processor := &rawCaptureProcessor{}
			reader := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
			if err := reader.Start(context.Background(), tt.ltx, processor, mustRegex(t, "HIT")); err != nil {
				t.Fatalf("context read: %v", err)
			}

			if len(reference.lines) == 0 {
				t.Fatal("reference read emitted no lines; the test would be vacuous")
			}
			wantRaw, wantOwned := len(reference.lines), 0
			if tt.ltx.BeforeContext > 0 {
				wantRaw, wantOwned = 0, len(reference.lines)
			}
			if processor.bufferCalls != wantOwned || processor.rawCalls != wantRaw {
				t.Fatalf("owned/raw calls = %d/%d, want %d/%d", processor.bufferCalls, processor.rawCalls, wantOwned, wantRaw)
			}
			if !equalStrings(processor.lines, reference.lines) {
				t.Fatalf("lines = %q, want %q", processor.lines, reference.lines)
			}
			if !equalUint64s(processor.lineNums, reference.lineNums) {
				t.Fatalf("line numbers = %v, want %v", processor.lineNums, reference.lineNums)
			}
		})
	}
}

// TestNewFilteringProcessorResolvesRawProcessor pins when the fast path is
// wired up: only for a processor implementing line.RawProcessor and only
// without before-context (max/after-only context does not retain input).
func TestNewFilteringProcessorResolvesRawProcessor(t *testing.T) {
	tests := []struct {
		name      string
		processor line.Processor
		ltx       lcontext.LContext
		wantRaw   bool
	}{
		{name: "raw processor without context", processor: &rawCaptureProcessor{}, wantRaw: true},
		{name: "raw processor with before context", processor: &rawCaptureProcessor{}, ltx: lcontext.LContext{BeforeContext: 1}},
		{name: "raw processor with after context", processor: &rawCaptureProcessor{}, ltx: lcontext.LContext{AfterContext: 1}, wantRaw: true},
		{name: "raw processor with max count", processor: &rawCaptureProcessor{}, ltx: lcontext.LContext{MaxCount: 1}, wantRaw: true},
		{name: "buffer-only processor", processor: &captureProcessor{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := newSnapshotReadFile(filepath.Join(t.TempDir(), "unused.log"), "glob-id",
				make(chan string, 1), defaultMaxLineLength, testLogger)
			fp := reader.newFilteringProcessor(tt.ltx, tt.processor, regex.NewNoop())
			if got := fp.rawProcessor != nil; got != tt.wantRaw {
				t.Fatalf("raw fast path wired = %v, want %v", got, tt.wantRaw)
			}
		})
	}
}

// TestProcessFilteredRawBorrowsWithoutPooledBuffer checks the fast path itself:
// the processor receives the caller's slice (no copy), nothing is allocated for
// a matching line, and a processor error is returned unchanged.
func TestProcessFilteredRawBorrowsWithoutPooledBuffer(t *testing.T) {
	processor := &rawCaptureProcessor{}
	var st stats
	fp := &filteringProcessor{
		processor:    processor,
		rawProcessor: processor,
		re:           regex.NewNoop(),
		stats:        &st,
		globID:       "glob-id",
	}

	raw := []byte("matching line\n")
	if err := fp.ProcessFilteredRaw(raw); err != nil {
		t.Fatalf("ProcessFilteredRaw: %v", err)
	}
	if processor.rawCalls != 1 || processor.bufferCalls != 0 {
		t.Fatalf("raw calls = %d, buffer calls = %d, want 1 and 0", processor.rawCalls, processor.bufferCalls)
	}
	if &processor.lastRaw[0] != &raw[0] {
		t.Fatal("fast path copied the line; want the caller's slice borrowed as is")
	}
	if st.matchCount != 1 || st.transmitCount != 1 {
		t.Fatalf("stats matched=%d transmitted=%d, want 1 and 1", st.matchCount, st.transmitCount)
	}

	// A fresh processor keeps the AllocsPerRun closure free of the recording
	// slices' growth; only the fast path itself is measured.
	counting := &countingRawProcessor{}
	fp.processor = counting
	fp.rawProcessor = counting
	allocs := testing.AllocsPerRun(100, func() {
		if err := fp.ProcessFilteredRaw(raw); err != nil {
			t.Fatalf("ProcessFilteredRaw: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("matching line on the raw fast path allocated %v times, want 0", allocs)
	}

	sinkErr := errors.New("write failed")
	processor.rawErr = sinkErr
	fp.processor = processor
	fp.rawProcessor = processor
	if err := fp.ProcessFilteredRaw(raw); !errors.Is(err, sinkErr) {
		t.Fatalf("ProcessFilteredRaw error = %v, want %v", err, sinkErr)
	}
}

// TestProcessFilteredRawFallbackCopiesIntoPooledBuffer is the negative case for
// a processor without the fast path: it must get its own buffer, which stays
// intact when the caller reuses the transient slice afterwards.
func TestProcessFilteredRawFallbackCopiesIntoPooledBuffer(t *testing.T) {
	processor := &retainingProcessor{}
	var st stats
	fp := &filteringProcessor{
		processor: processor,
		re:        regex.NewNoop(),
		stats:     &st,
		globID:    "glob-id",
	}

	raw := []byte("matching line\n")
	if err := fp.ProcessFilteredRaw(raw); err != nil {
		t.Fatalf("ProcessFilteredRaw: %v", err)
	}
	copy(raw, "XXXXXXXXXXXXXX")

	if len(processor.bufs) != 1 {
		t.Fatalf("processor received %d buffers, want 1", len(processor.bufs))
	}
	if got := processor.bufs[0].String(); got != "matching line\n" {
		t.Fatalf("buffer content = %q after the caller reused its slice, want the original line", got)
	}
	pool.RecycleBytesBuffer(processor.bufs[0])
}

// TestTailUsesRawProcessorFastPath covers the follow reader, whose borrowed
// slice is the reused partial-line buffer rather than a scanner token.
func TestTailUsesRawProcessorFastPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	writeTestPath(t, path, "historical\n")

	tail := newFollowReadFile(path, "follow.log", nil, defaultMaxLineLength, testLogger)
	opened := make(chan struct{}, 1)
	tail.truncateCheck = func(ctx context.Context, _ chan<- struct{}) {
		opened <- struct{}{}
		<-ctx.Done()
	}

	processor := &rawTailCaptureProcessor{tailCaptureProcessor: tailCaptureProcessor{lines: make(chan string, 8)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tail.Start(ctx, lcontext.LContext{}, processor, mustRegex(t, "live"))
	}()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not open the file")
	}

	appendTestPath(t, path, "live one\nskipped\nlive two, a longer line\nlive 3\n")
	for _, want := range []string{"live one", "live two, a longer line", "live 3"} {
		assertTailLine(t, processor.lines, want)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tail returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not stop after cancellation")
	}
	if got := processor.bufferCalls.Load(); got != 0 {
		t.Fatalf("ProcessLine called %d times in follow mode, want 0", got)
	}
	if got := processor.rawCalls.Load(); got != 3 {
		t.Fatalf("ProcessRawLine called %d times in follow mode, want 3", got)
	}
}

// countingRawProcessor is an allocation-free RawProcessor for AllocsPerRun.
type countingRawProcessor struct {
	lines int
}

func (p *countingRawProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	pool.RecycleBytesBuffer(buf)
	return nil
}

func (p *countingRawProcessor) ProcessRawLine(_ []byte, _ uint64, _ string) error {
	p.lines++
	return nil
}

func (p *countingRawProcessor) Flush() error { return nil }
func (p *countingRawProcessor) Close() error { return nil }

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func equalUint64s(a, b []uint64) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
