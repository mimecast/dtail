package fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// lineFilterTestContent mixes matching, non-matching and empty lines and ends
// without a newline, so every branch of the snapshot split function is used.
const lineFilterTestContent = "INFO one\nERROR two\n\nINFO three\nINFO four\n" +
	"ERROR five\nINFO six\nERROR seven\nINFO eight\nINFO nine\nERROR ten"

// filterCaptureProcessor records lines like captureProcessor, but also offers the
// line.RawProcessor fast path and records which path each line took.
type filterCaptureProcessor struct {
	captureProcessor
	rawLines int
	sources  []string
	restarts int
	flushes  int
}

func (p *filterCaptureProcessor) ProcessRawLine(raw []byte, lineNum uint64, sourceID string) error {
	p.rawLines++
	p.lines = append(p.lines, string(raw))
	p.lineNums = append(p.lineNums, lineNum)
	p.sources = append(p.sources, sourceID)
	return nil
}

func (p *filterCaptureProcessor) ProcessLine(lineContent *bytes.Buffer, lineNum uint64, sourceID string) error {
	p.sources = append(p.sources, sourceID)
	return p.captureProcessor.ProcessLine(lineContent, lineNum, sourceID)
}

func (p *filterCaptureProcessor) Flush() error {
	p.flushes++
	return p.captureProcessor.Flush()
}

func (p *filterCaptureProcessor) SourceRestarted() {
	p.restarts++
}

func mustFlaggedRegex(t *testing.T, pattern string, flag regex.Flag) regex.Regex {
	t.Helper()
	re, err := regex.New(pattern, flag)
	if err != nil {
		t.Fatalf("regex.New(%q) error = %v", pattern, err)
	}
	return re
}

// snapshotTokens splits content the way a private snapshot reader does, so a
// LineFilter can be fed exactly the lines ReadFile.Start filters.
func snapshotTokens(t *testing.T, content string) [][]byte {
	t.Helper()
	splitter := newSnapshotReadFile("", "glob", nil, defaultMaxLineLength, testLogger)
	scanner := bufio.NewScanner(bytes.NewReader([]byte(content)))
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		return splitter.scanLinesWithMaxLength(context.Background(), data, atEOF)
	})
	var tokens [][]byte
	for scanner.Scan() {
		tokens = append(tokens, append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return tokens
}

// feedLineFilter feeds every token, stopping like a reader when the filter
// reports a max-count stop, then flushes and closes the filter.
func feedLineFilter(t *testing.T, filter *LineFilter, tokens [][]byte) {
	t.Helper()
	for _, token := range tokens {
		stop, err := filter.ProcessLine(token)
		if err != nil {
			t.Fatalf("ProcessLine(%q) error = %v", token, err)
		}
		if stop {
			break
		}
	}
	if err := filter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	filter.Close()
}

func TestLineFilterMatchesPrivateSnapshotRead(t *testing.T) {
	contexts := []struct {
		name string
		ltx  lcontext.LContext
	}{
		{"no context", lcontext.LContext{}},
		{"before 2", lcontext.LContext{BeforeContext: 2}},
		{"after 1", lcontext.LContext{AfterContext: 1}},
		{"before 1 after 1", lcontext.LContext{BeforeContext: 1, AfterContext: 1}},
		{"max 2", lcontext.LContext{MaxCount: 2}},
		{"max 1 after 2", lcontext.LContext{MaxCount: 1, AfterContext: 2}},
	}
	patterns := []struct {
		name    string
		pattern string
		flag    regex.Flag
	}{
		{"noop", "", regex.Default},
		{"ERROR", "ERROR", regex.Default},
		{"invert ERROR", "ERROR", regex.Invert},
	}

	filePath := writeProcessorTestFile(t, lineFilterTestContent)
	tokens := snapshotTokens(t, lineFilterTestContent)

	for _, ctxCase := range contexts {
		for _, patternCase := range patterns {
			t.Run(ctxCase.name+"/"+patternCase.name, func(t *testing.T) {
				re := mustFlaggedRegex(t, patternCase.pattern, patternCase.flag)

				private := &captureProcessor{}
				reader := newSnapshotReadFile(filePath, "glob", nil, defaultMaxLineLength, testLogger)
				if err := reader.Start(context.Background(), ctxCase.ltx, private, re); err != nil {
					t.Fatalf("private read error = %v", err)
				}
				if len(private.lines) == 0 {
					t.Fatal("test setup: the private read produced no lines")
				}

				shared := &captureProcessor{}
				feedLineFilter(t, NewLineFilter(ctxCase.ltx, shared, re, "glob"), tokens)

				if !reflect.DeepEqual(shared.lines, private.lines) {
					t.Errorf("lines = %q, want %q", shared.lines, private.lines)
				}
				if !reflect.DeepEqual(shared.lineNums, private.lineNums) {
					t.Errorf("line numbers = %v, want %v", shared.lineNums, private.lineNums)
				}
			})
		}
	}
}

func TestLineFilterUsesRawFastPathWithoutBeforeContext(t *testing.T) {
	tests := []struct {
		name     string
		ltx      lcontext.LContext
		wantRaw  bool
		wantNums []uint64
	}{
		{"no context", lcontext.LContext{}, true, []uint64{2}},
		{"after context", lcontext.LContext{AfterContext: 1}, true, []uint64{2, 3}},
		{"max context", lcontext.LContext{MaxCount: 1}, true, []uint64{2}},
		{"before context", lcontext.LContext{BeforeContext: 1}, false, []uint64{1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &filterCaptureProcessor{}
			filter := NewLineFilter(tt.ltx, processor, mustRegex(t, "ERROR"), "src")
			feedLineFilter(t, filter, [][]byte{[]byte("INFO a"), []byte("ERROR b"), []byte("INFO c")})

			if gotRaw := processor.rawLines > 0; gotRaw != tt.wantRaw {
				t.Errorf("raw fast path used = %v, want %v", gotRaw, tt.wantRaw)
			}
			if !reflect.DeepEqual(processor.lineNums, tt.wantNums) {
				t.Errorf("line numbers = %v, want %v", processor.lineNums, tt.wantNums)
			}
			for _, source := range processor.sources {
				if source != "src" {
					t.Errorf("source ID = %q, want %q", source, "src")
				}
			}
		})
	}
}

func TestLineFiltersNumberLinesIndependently(t *testing.T) {
	re := regex.NewNoop()
	first := &captureProcessor{}
	second := &captureProcessor{}
	firstFilter := NewLineFilter(lcontext.LContext{}, first, re, "glob")
	secondFilter := NewLineFilter(lcontext.LContext{}, second, re, "glob")

	// The second filter joins after the first has seen two lines, as a
	// session joining a shared read later would.
	for _, raw := range []string{"a", "b"} {
		if _, err := firstFilter.ProcessLine([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"c", "d"} {
		for _, filter := range []*LineFilter{firstFilter, secondFilter} {
			if _, err := filter.ProcessLine([]byte(raw)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if want := []uint64{1, 2, 3, 4}; !reflect.DeepEqual(first.lineNums, want) {
		t.Errorf("first filter line numbers = %v, want %v", first.lineNums, want)
	}
	if want := []uint64{1, 2}; !reflect.DeepEqual(second.lineNums, want) {
		t.Errorf("second filter line numbers = %v, want %v", second.lineNums, want)
	}
}

func TestLineFilterDoesNotRetainRawLine(t *testing.T) {
	for _, ltx := range []lcontext.LContext{{}, {BeforeContext: 1}} {
		processor := &captureProcessor{}
		filter := NewLineFilter(ltx, processor, mustRegex(t, "ERROR"), "glob")

		// One reused backing array, as a shared reader's chunk would be.
		raw := []byte("INFO before")
		if _, err := filter.ProcessLine(raw); err != nil {
			t.Fatal(err)
		}
		copy(raw, "XXXXXXXXXXX")
		raw = append(raw[:0], "ERROR match"...)
		if _, err := filter.ProcessLine(raw); err != nil {
			t.Fatal(err)
		}
		copy(raw, "XXXXXXXXXXX")

		want := []string{"ERROR match"}
		if ltx.BeforeContext > 0 {
			want = []string{"INFO before", "ERROR match"}
		}
		if !reflect.DeepEqual(processor.lines, want) {
			t.Errorf("context %+v: lines = %q, want %q", ltx, processor.lines, want)
		}
	}
}

func TestLineFilterReportsMaxCountStop(t *testing.T) {
	// As in the private reader, the match that reaches the limit is still
	// emitted and ends the read at once when there is no after context.
	processor := &captureProcessor{}
	filter := NewLineFilter(lcontext.LContext{MaxCount: 2}, processor, mustRegex(t, "ERROR"), "glob")

	for i, step := range []struct {
		raw      string
		wantStop bool
	}{
		{"ERROR first", false},
		{"INFO between", false},
		{"ERROR second", true},
	} {
		stop, err := filter.ProcessLine([]byte(step.raw))
		if stop != step.wantStop || err != nil {
			t.Fatalf("line %d %q: stop = %v, err = %v, want %v, nil", i, step.raw, stop, err, step.wantStop)
		}
	}
	if want := []string{"ERROR first", "ERROR second"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want %q", processor.lines, want)
	}
}

func TestLineFilterPropagatesProcessorErrors(t *testing.T) {
	processErr := errors.New("client went away")
	tests := []struct {
		name string
		ltx  lcontext.LContext
	}{
		{"no context", lcontext.LContext{}},
		{"before context", lcontext.LContext{BeforeContext: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &captureProcessor{errAtLine: 1, processErr: processErr}
			filter := NewLineFilter(tt.ltx, processor, regex.NewNoop(), "glob")
			stop, err := filter.ProcessLine([]byte("line"))
			if stop || !errors.Is(err, processErr) {
				t.Fatalf("ProcessLine() = %v, %v, want false, %v", stop, err, processErr)
			}

			processor.flushErr = processErr
			if err := filter.Flush(); !errors.Is(err, processErr) {
				t.Fatalf("Flush() error = %v, want %v", err, processErr)
			}
		})
	}
}

func TestLineFilterRestartDiscardsContextAndNotifiesProcessor(t *testing.T) {
	processor := &filterCaptureProcessor{}
	filter := NewLineFilter(lcontext.LContext{BeforeContext: 1}, processor,
		mustRegex(t, "ERROR"), "glob")

	if _, err := filter.ProcessLine([]byte("INFO old content")); err != nil {
		t.Fatal(err)
	}
	filter.Restart()
	if _, err := filter.ProcessLine([]byte("ERROR new content")); err != nil {
		t.Fatal(err)
	}

	// The before-context line from the old content must not be emitted, the
	// processor learns that its source starts over, and line numbers go on.
	if want := []string{"ERROR new content"}; !reflect.DeepEqual(processor.lines, want) {
		t.Errorf("lines = %q, want %q", processor.lines, want)
	}
	if want := []uint64{2}; !reflect.DeepEqual(processor.lineNums, want) {
		t.Errorf("line numbers = %v, want %v", processor.lineNums, want)
	}
	if processor.restarts != 1 {
		t.Errorf("SourceRestarted calls = %d, want 1", processor.restarts)
	}
}

func TestLineFilterCloseReleasesBufferedContext(t *testing.T) {
	var recycled []*bytes.Buffer
	processor := &captureProcessor{}
	filter := NewLineFilter(lcontext.LContext{BeforeContext: 2}, processor,
		mustRegex(t, "ERROR"), "glob")
	filter.filter.recycle = func(buf *bytes.Buffer) {
		recycled = append(recycled, buf)
		pool.RecycleBytesBuffer(buf)
	}

	for _, raw := range []string{"INFO a", "INFO b"} {
		if _, err := filter.ProcessLine([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	filter.Close()

	if len(recycled) != 2 {
		t.Errorf("recycled %d buffered context lines on Close, want 2", len(recycled))
	}
	if len(processor.lines) != 0 {
		t.Errorf("Close emitted lines %q, want none", processor.lines)
	}
}

func TestLineFilterReopenMatchesPrivateReaderRestart(t *testing.T) {
	// A private reader is started again on the same ReadFile when the read
	// command retries, e.g. after a rotation: numbering carries on while the
	// max-count and local context start afresh. Reopen must do the same.
	contexts := []lcontext.LContext{
		{},
		{MaxCount: 1},
		{BeforeContext: 1, AfterContext: 1},
	}
	filePath := writeProcessorTestFile(t, lineFilterTestContent)
	tokens := snapshotTokens(t, lineFilterTestContent)
	re := mustRegex(t, "ERROR")

	for _, ltx := range contexts {
		reader := newSnapshotReadFile(filePath, "glob", nil, defaultMaxLineLength, testLogger)
		firstFilter := NewLineFilter(ltx, nil, re, "glob")

		for round := 1; round <= 2; round++ {
			private := &captureProcessor{}
			if err := reader.Start(context.Background(), ltx, private, re); err != nil {
				t.Fatalf("%+v round %d: private read error = %v", ltx, round, err)
			}

			shared := &captureProcessor{}
			firstFilter.Reopen(shared)
			for _, token := range tokens {
				stop, err := firstFilter.ProcessLine(token)
				if err != nil {
					t.Fatal(err)
				}
				if stop {
					break
				}
			}

			if !reflect.DeepEqual(shared.lines, private.lines) ||
				!reflect.DeepEqual(shared.lineNums, private.lineNums) {
				t.Errorf("%+v round %d: got %q %v, want %q %v", ltx, round,
					shared.lines, shared.lineNums, private.lines, private.lineNums)
			}
		}
	}
}

func TestLineFilterReopenReleasesBufferedContext(t *testing.T) {
	var recycled int
	filter := NewLineFilter(lcontext.LContext{BeforeContext: 1}, &captureProcessor{},
		mustRegex(t, "ERROR"), "glob")
	filter.filter.recycle = func(buf *bytes.Buffer) {
		recycled++
		pool.RecycleBytesBuffer(buf)
	}
	if _, err := filter.ProcessLine([]byte("INFO buffered")); err != nil {
		t.Fatal(err)
	}

	next := &captureProcessor{}
	filter.Reopen(next)
	if recycled != 1 {
		t.Errorf("Reopen recycled %d buffered context lines, want 1", recycled)
	}
	// The before-context line of the previous read must not reappear.
	if _, err := filter.ProcessLine([]byte("ERROR next")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ERROR next"}; !reflect.DeepEqual(next.lines, want) {
		t.Errorf("lines after Reopen = %q, want %q", next.lines, want)
	}
	// The recycle seam survives Reopen.
	if _, err := filter.ProcessLine([]byte("INFO again")); err != nil {
		t.Fatal(err)
	}
	filter.Close()
	if recycled != 2 {
		t.Errorf("recycled %d lines in total, want 2", recycled)
	}
}
