package fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

type captureProcessor struct {
	lines      []string
	lineNums   []uint64
	errAtLine  int
	processErr error
	flushErr   error
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func (p *captureProcessor) ProcessLine(lineContent *bytes.Buffer, lineNum uint64, _ string) error {
	p.lines = append(p.lines, lineContent.String())
	p.lineNums = append(p.lineNums, lineNum)
	pool.RecycleBytesBuffer(lineContent)

	if p.errAtLine > 0 && len(p.lines) == p.errAtLine {
		return p.processErr
	}
	return nil
}

func (p *captureProcessor) Flush() error {
	return p.flushErr
}

func (p *captureProcessor) Close() error {
	return nil
}

func TestStartReadsAllLines(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\nbeta\n")
	re := regex.NewNoop()

	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	processor := &captureProcessor{}

	if err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		re,
	); err != nil {
		t.Fatalf("reader start failed: %v", err)
	}

	want := []string{"alpha\n", "beta\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, want)
	}
}

func TestStartPropagatesTruncateChildPanic(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\n")
	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	cat.truncateCheck = func(context.Context, chan<- struct{}) {
		panic("truncate child failed")
	}
	err := cat.Start(context.Background(), lcontext.LContext{}, &captureProcessor{}, regex.NewNoop())
	if !errors.Is(err, ErrReaderWorkerPanic) || !strings.Contains(err.Error(), "truncate child failed") {
		t.Fatalf("reader error = %v, want propagated truncate child panic", err)
	}
}

func TestStartTruncateChildPanicUnblocksPipeRead(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create input pipe: %v", err)
	}
	defer func() { _ = input.Close() }()
	defer func() { _ = writer.Close() }()

	reader := newSnapshotReadFile("", "-", make(chan string, 1), defaultMaxLineLength, testLogger)
	reader.pipeInput = input
	reader.truncateCheck = func(context.Context, chan<- struct{}) {
		panic("truncate child failed while pipe blocked")
	}

	done := make(chan error, 1)
	go func() {
		done <- reader.Start(context.Background(), lcontext.LContext{},
			&captureProcessor{}, regex.NewNoop())
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrReaderWorkerPanic) {
			t.Fatalf("reader error = %v, want worker panic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker panic did not unblock pipe read")
	}
}

func TestServerlessPipeReadReturnsOnCancellationWithoutClosingInput(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create input pipe: %v", err)
	}
	defer func() { _ = input.Close() }()
	defer func() { _ = writer.Close() }()

	processed := make(chan string, 1)
	flushed := make(chan struct{}, 1)
	processor := &signalingPipeProcessor{processed: processed, flushed: flushed}
	reader := newSnapshotReadFile("", "-", make(chan string, 1), defaultMaxLineLength, testLogger)
	reader.pipeInput = input

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop())
	}()

	if _, err := writer.WriteString("complete line\n"); err != nil {
		t.Fatalf("write input: %v", err)
	}
	select {
	case got := <-processed:
		if got != "complete line\n" {
			t.Fatalf("processed line = %q, want complete input line", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipe input was not processed")
	}

	// Keep the write side open and idle. Cancellation must wake the
	// platform-specific reader rather than waiting for writer EOF.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled pipe read returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled pipe read stayed blocked on held-open input")
	}
	select {
	case <-flushed:
	default:
		t.Fatal("processor was not flushed before canceled pipe read returned")
	}

	// The reader borrows stdin; teardown must not close the caller-owned file.
	if _, err := writer.WriteString("input remains open\n"); err != nil {
		t.Fatalf("pipe input was closed by canceled read: %v", err)
	}
}

type signalingPipeProcessor struct {
	processed chan<- string
	flushed   chan<- struct{}
}

func (p *signalingPipeProcessor) ProcessLine(content *bytes.Buffer, _ uint64, _ string) error {
	p.processed <- content.String()
	pool.RecycleBytesBuffer(content)
	return nil
}

func (p *signalingPipeProcessor) Flush() error {
	select {
	case p.flushed <- struct{}{}:
	default:
	}
	return nil
}

func (*signalingPipeProcessor) Close() error { return nil }

// TestReadWithProcessorOptimizedDetectsTruncation proves that after the
// per-line time.Since truncate gate was removed (task 2t0), the non-follow
// read loop still detects truncation: when the periodicTruncateCheck goroutine
// signals on the truncate channel, the loop re-stats the file and returns the
// truncation error. The reader (line source) is decoupled from the fd (stat
// source) so the scenario is deterministic without relying on the 3s cadence:
// the file on disk is shorter than the fd's current read position, exactly the
// state truncated() flags. A signal is pre-loaded on the truncate channel so
// the very first loop iteration performs the check.
func TestReadWithProcessorOptimizedDetectsTruncation(t *testing.T) {
	// The on-disk file is intentionally tiny; the fd is then seeked well past
	// its end to emulate having read a file that shrank underneath us.
	filePath := writeProcessorTestFile(t, "short")

	fd, openErr := os.Open(filePath)
	if openErr != nil {
		t.Fatalf("open file: %v", openErr)
	}
	t.Cleanup(func() {
		if closeErr := fd.Close(); closeErr != nil {
			t.Errorf("close test file: %v", closeErr)
		}
	})
	if _, err := fd.Seek(4096, 0); err != nil {
		t.Fatalf("seek fd past end: %v", err)
	}

	// The scanner reads its lines from an independent in-memory reader so the
	// loop actually iterates and reaches the truncate check.
	reader := bufio.NewReader(strings.NewReader("l1\nl2\nl3\nl4\nl5\n"))

	// Pre-load one truncate signal (buffered) so the first iteration checks.
	truncate := make(chan struct{}, 1)
	truncate <- struct{}{}

	rf := ReadFile{
		logger:        testLogger,
		filePath:      filePath,
		globID:        "glob-id",
		maxLineLength: defaultMaxLineLength,
	}

	readErr := rf.readWithProcessorOptimized(
		context.Background(),
		fd,
		reader,
		truncate,
		rf.newFilteringProcessor(lcontext.LContext{}, &captureProcessor{}, regex.NewNoop()),
	)
	if readErr == nil {
		t.Fatal("expected truncation to be detected, got nil error")
	}
	if !strings.Contains(readErr.Error(), "truncated") {
		t.Fatalf("expected truncation error, got: %v", readErr)
	}
}

func TestStartReturnsOpenError(t *testing.T) {
	re := regex.NewNoop()
	missingFile := filepath.Join(t.TempDir(), "missing.log")

	cat := newSnapshotReadFile(missingFile, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	if err := cat.Start(context.Background(), lcontext.LContext{}, &captureProcessor{}, re); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestStartPropagatesProcessError(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\nbeta\n")
	re := regex.NewNoop()
	expectedErr := errors.New("processor failure")

	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	processor := &captureProcessor{
		errAtLine:  1,
		processErr: expectedErr,
	}

	err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		re,
	)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected process error %v, got %v", expectedErr, err)
	}
}

func TestStartUsesInjectedMaxLineLength(t *testing.T) {
	filePath := writeProcessorTestFile(t, "abcdef\n")
	re := regex.NewNoop()

	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), 3, testLogger)
	processor := &captureProcessor{}

	if err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		re,
	); err != nil {
		t.Fatalf("reader start failed: %v", err)
	}

	want := []string{"abc", "def\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, want)
	}
}

func TestStartWaitsOnLiveLongLineWarningUntilCanceled(t *testing.T) {
	filePath := writeProcessorTestFile(t, strings.Repeat("a", 8))
	re := regex.NewNoop()

	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string), 1, testLogger)
	processor := &captureProcessor{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- cat.Start(
			ctx,
			lcontext.LContext{},
			processor,
			re,
		)
	}()

	select {
	case err := <-done:
		t.Fatalf("reader returned before cancellation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled reader to stop with nil or context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not return after cancellation")
	}
}

// TestStartExitsWhenContextCanceledDuringLongLineWarning proves Start returns
// cleanly when its context is already canceled while a long-line warning would
// otherwise block. TestStartWaitsOnLiveLongLineWarningUntilCanceled covers
// cancellation after reading begins.
func TestStartExitsWhenContextCanceledDuringLongLineWarning(t *testing.T) {
	filePath := writeProcessorTestFile(t, strings.Repeat("a", 8))
	re := regex.NewNoop()

	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string), 1, testLogger)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- cat.Start(ctx, lcontext.LContext{}, &captureProcessor{}, re)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected canceled start to exit cleanly, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("start did not return after context cancellation")
	}
}

func TestTailWithProcessorOptimizedExitsWhenContextCanceledDuringLongLineWarning(t *testing.T) {
	filePath := writeProcessorTestFile(t, strings.Repeat("a", 8))
	re := regex.NewNoop()

	rf := ReadFile{
		logger:         testLogger,
		filePath:       filePath,
		globID:         "glob-id",
		serverMessages: make(chan string),
		retry:          true,
		canSkipLines:   true,
		follow:         true,
		maxLineLength:  1,
	}

	reader, fd, decompressor, err := rf.makeReader(context.Background())
	if fd != nil {
		t.Cleanup(func() {
			if closeErr := fd.Close(); closeErr != nil {
				t.Errorf("close test file: %v", closeErr)
			}
		})
	}
	if decompressor != nil {
		defer func() {
			if closeErr := decompressor.Close(); closeErr != nil {
				t.Fatalf("unable to close decompressor: %v", closeErr)
			}
		}()
	}
	if err != nil {
		t.Fatalf("make reader: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- rf.tailWithProcessorOptimized(
			ctx,
			fd,
			reader,
			make(chan struct{}),
			rf.newFilteringProcessor(lcontext.LContext{}, &captureProcessor{}, re),
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected canceled optimized tail to exit cleanly, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("optimized tail did not return after context cancellation")
	}
}

func TestTailWithProcessorOptimizedRecognizesWrappedEOF(t *testing.T) {
	rf := ReadFile{
		logger:        testLogger,
		filePath:      "test.log",
		globID:        "glob-id",
		maxLineLength: defaultMaxLineLength,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := rf.tailWithProcessorOptimized(
		ctx,
		nil,
		bufio.NewReader(errorReader{err: fmt.Errorf("read tail: %w", io.EOF)}),
		make(chan struct{}),
		rf.newFilteringProcessor(lcontext.LContext{}, &captureProcessor{}, regex.NewNoop()),
	)
	if err != nil {
		t.Fatalf("tailWithProcessorOptimized returned wrapped EOF: %v", err)
	}
}

func TestTailWithProcessorOptimizedPropagatesNonEOFReadError(t *testing.T) {
	wantErr := errors.New("read failed")
	rf := ReadFile{
		logger:        testLogger,
		filePath:      "test.log",
		globID:        "glob-id",
		maxLineLength: defaultMaxLineLength,
	}

	err := rf.tailWithProcessorOptimized(
		context.Background(),
		nil,
		bufio.NewReader(errorReader{err: wantErr}),
		make(chan struct{}),
		rf.newFilteringProcessor(lcontext.LContext{}, &captureProcessor{}, regex.NewNoop()),
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tailWithProcessorOptimized error = %v, want %v", err, wantErr)
	}
}

func TestIsEarlyStopRequiresExactEOFSentinel(t *testing.T) {
	if !isEarlyStop(io.EOF) {
		t.Fatal("isEarlyStop did not recognize bare io.EOF")
	}
	if isEarlyStop(fmt.Errorf("downstream write: %w", io.EOF)) {
		t.Fatal("isEarlyStop treated wrapped io.EOF as the private early-stop sentinel")
	}
}

// TestStartDoesNotDoubleRecycle verifies that ownership transfer from the read
// path to the processor does not put the same *bytes.Buffer into the pool twice.
// The trailing partial line covers the final, non-newline-terminated token.
func TestStartDoesNotDoubleRecycle(t *testing.T) {
	drainBytesBufferPool()

	filePath := writeProcessorTestFile(t, "alpha\nbeta")
	re := regex.NewNoop()

	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	processor := &captureProcessor{}

	if err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		re,
	); err != nil {
		t.Fatalf("reader start failed: %v", err)
	}

	want := []string{"alpha\n", "beta"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, want)
	}

	seen := make(map[*bytes.Buffer]int)
	for i := 0; i < 512; i++ {
		b := pool.BytesBuffer.Get().(*bytes.Buffer)
		seen[b]++
		if seen[b] > 1 {
			t.Fatalf("buffer %p observed in pool more than once: "+
				"double-recycle detected (Put twice into sync.Pool)", b)
		}
	}
}

// drainBytesBufferPool empties the global buffer pool of any previously-Put
// entries so that pool inspection in a subsequent test is not polluted by
// artifacts from earlier test runs.
func drainBytesBufferPool() {
	for i := 0; i < 1024; i++ {
		_ = pool.BytesBuffer.Get()
	}
}

// TestReadWithProcessorOptimizedFastPathByteIdentical proves that the no-context
// zero-copy fast path (match on scanner.Bytes() before copying) yields exactly
// the same emitted lines as the previous copy-every-line behavior, across grep
// hit rates, inverted matching, zero matches, and the cat noop (match-all) case.
func TestReadWithProcessorOptimizedFastPathByteIdentical(t *testing.T) {
	const content = "apple\nbanana\napricot\ncherry\navocado\n"

	mustRegex := func(pattern string, flag regex.Flag) regex.Regex {
		re, err := regex.New(pattern, flag)
		if err != nil {
			t.Fatalf("build regex %q: %v", pattern, err)
		}
		return re
	}

	// wantNums, when non-nil, pins the exact lineNum argument passed to
	// ProcessLine for each emitted line. Because f.updatePosition() runs for
	// every scanned line (matching or not) before the filter, non-matching lines
	// still advance the counter, so a match after N non-matches must report
	// lineNum N+1 (1-based) - never restarting at 1. This locks in that the
	// zero-copy fast path counts lines identically to the old copy-every-line
	// path. (apple=1, banana=2, apricot=3, cherry=4, avocado=5.)
	tests := []struct {
		name     string
		re       regex.Regex
		want     []string
		wantNums []uint64
	}{
		{
			name:     "low hit default",
			re:       mustRegex("ap", regex.Default),
			want:     []string{"apple\n", "apricot\n"},
			wantNums: []uint64{1, 3},
		},
		{
			name: "high hit default",
			re:   mustRegex("a", regex.Default),
			want: []string{"apple\n", "banana\n", "apricot\n", "avocado\n"},
		},
		{
			name: "zero match",
			re:   mustRegex("zzz", regex.Default),
			want: nil,
		},
		{
			name:     "invert",
			re:       mustRegex("ap", regex.Invert),
			want:     []string{"banana\n", "cherry\n", "avocado\n"},
			wantNums: []uint64{2, 4, 5},
		},
		{
			name: "noop matches all (cat)",
			re:   regex.NewNoop(),
			want: []string{"apple\n", "banana\n", "apricot\n", "cherry\n", "avocado\n"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := writeProcessorTestFile(t, content)
			cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
			processor := &captureProcessor{}

			if err := cat.Start(
				context.Background(),
				lcontext.LContext{},
				processor,
				tt.re,
			); err != nil {
				t.Fatalf("reader start failed: %v", err)
			}

			if !reflect.DeepEqual(processor.lines, tt.want) {
				t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, tt.want)
			}
			if tt.wantNums != nil && !reflect.DeepEqual(processor.lineNums, tt.wantNums) {
				t.Fatalf("unexpected line numbers: got=%v want=%v", processor.lineNums, tt.wantNums)
			}
		})
	}
}

// TestReadWithProcessorOptimizedContextPathUnchanged exercises the local-context
// path (ltx.Has() == true), which must keep buffering every line so before/after
// context lines are still emitted. The fast path must NOT be taken here.
func TestReadWithProcessorOptimizedContextPathUnchanged(t *testing.T) {
	const content = "a\nb\nHIT\nd\ne\n"
	re, err := regex.New("HIT", regex.Default)
	if err != nil {
		t.Fatalf("build regex: %v", err)
	}

	filePath := writeProcessorTestFile(t, content)
	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	processor := &captureProcessor{}

	// One line of before context and one line of after context around the match.
	ltx := lcontext.LContext{BeforeContext: 1, AfterContext: 1}
	if err := cat.Start(
		context.Background(),
		ltx,
		processor,
		re,
	); err != nil {
		t.Fatalf("reader start failed: %v", err)
	}

	want := []string{"b\n", "HIT\n", "d\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected context lines: got=%v want=%v", processor.lines, want)
	}
}

// TestProcessFilteredRawZeroAllocOnNonMatch locks in the win: a non-matching line
// on the fast path must not acquire a pooled buffer or copy anything, so it
// allocates nothing. A matching line does allocate (buffer copy + emit).
func TestProcessFilteredRawZeroAllocOnNonMatch(t *testing.T) {
	re, err := regex.New("MATCHME", regex.Default)
	if err != nil {
		t.Fatalf("build regex: %v", err)
	}

	var st stats
	fp := &filteringProcessor{
		processor: &captureProcessor{},
		re:        re,
		ltx:       lcontext.LContext{},
		stats:     &st,
		globID:    "glob-id",
	}

	nonMatch := []byte("this line does not contain the needle\n")
	allocs := testing.AllocsPerRun(100, func() {
		if err := fp.ProcessFilteredRaw(nonMatch); err != nil {
			t.Fatalf("ProcessFilteredRaw returned error: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("expected zero allocations on non-matching fast-path line, got %v", allocs)
	}
}

// TestStartMaxCountEarlyStopNoErrorLeak is a regression test for the read path
// leaking the io.EOF early-stop sentinel that
// filteringProcessor.processWithContext returns once a -m/-max (MaxCount) limit
// is reached. Start must return nil and emit the expected lines, proving the
// sentinel is handled as a clean early stop instead of a genuine I/O error.
func TestStartMaxCountEarlyStopNoErrorLeak(t *testing.T) {
	const content = "match 1\nother\nmatch 2\nother\nmatch 3\nother\nmatch 4\n"
	re, err := regex.New("match", regex.Default)
	if err != nil {
		t.Fatalf("build regex: %v", err)
	}
	// MaxCount without after-context: processWithContext returns io.EOF as soon
	// as the second match is emitted (the -max 2 early stop).
	ltx := lcontext.LContext{MaxCount: 2}

	filePath := writeProcessorTestFile(t, content)
	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	processor := &captureProcessor{}
	if err := cat.Start(context.Background(), ltx, processor, re); err != nil {
		// A non-nil return here is exactly the leaked sentinel the server
		// would log as ERROR|...|EOF.
		t.Fatalf("reader returned error; max-count early-stop sentinel must be swallowed: %v", err)
	}

	want := []string{"match 1\n", "match 2\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("-max lines: got=%v want=%v", processor.lines, want)
	}
}

// TestStartMaxCountWithContextEarlyStop covers -m combined
// with after-context (-A). Here processWithContext returns the io.EOF sentinel
// from its maxReached branch (a distinct return site from plain -m: it fires on
// the NEXT match after the after-context window drains, not on the match that
// reaches the count). Start must swallow the sentinel (return nil) and emit the
// after-context line.
func TestStartMaxCountWithContextEarlyStop(t *testing.T) {
	const content = "x\nHIT one\ny\nHIT two\nz\nHIT three\n"
	// MaxCount 1 with AfterContext 1: emit the first match plus its single
	// trailing context line, then stop at the next match via the maxReached
	// sentinel.
	ltx := lcontext.LContext{MaxCount: 1, AfterContext: 1}

	re, err := regex.New("HIT", regex.Default)
	if err != nil {
		t.Fatalf("build regex: %v", err)
	}
	filePath := writeProcessorTestFile(t, content)
	cat := newSnapshotReadFile(filePath, "glob-id", make(chan string, 1), defaultMaxLineLength, testLogger)
	processor := &captureProcessor{}
	if err := cat.Start(context.Background(), ltx, processor, re); err != nil {
		t.Fatalf("reader returned error; max-count+context sentinel must be swallowed: %v", err)
	}

	want := []string{"HIT one\n", "y\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("-m+context lines: got=%v want=%v", processor.lines, want)
	}
}

// TestTailWithProcessorOptimizedMaxCountEarlyStop proves the follow/tail
// optimized path (tailWithProcessorOptimized) treats the io.EOF max-count early-stop
// sentinel as a clean stop (return nil) at ALL THREE of its processPartialLine
// call sites: the newline-terminated line site, the long-line split site, and
// the context-cancel trailing-partial cleanup site. Pre-fix each site returned
// io.EOF straight to the caller (logged as SERVER|...|ERROR|...|EOF), so every
// subtest goes red on the unfixed code. The reader is driven in-memory so the
// follow loop is deterministic; serverMessages is nil so warnAboutLongLine never
// blocks (it returns true immediately).
func TestTailWithProcessorOptimizedMaxCountEarlyStop(t *testing.T) {
	newReadFile := func(maxLineLength int) ReadFile {
		return ReadFile{
			logger:        testLogger,
			filePath:      "test.log",
			globID:        "glob-id",
			maxLineLength: maxLineLength,
		}
	}

	mustRegex := func(pattern string) regex.Regex {
		re, err := regex.New(pattern, regex.Default)
		if err != nil {
			t.Fatalf("build regex %q: %v", pattern, err)
		}
		return re
	}

	// runTail drives tailWithProcessorOptimized directly. fd is nil because the
	// truncate channel is never signaled, so f.truncated(fd) is never reached.
	runTail := func(t *testing.T, ctx context.Context, rf *ReadFile, input string,
		ltx lcontext.LContext, re regex.Regex) *captureProcessor {

		processor := &captureProcessor{}
		reader := bufio.NewReader(strings.NewReader(input))
		if err := rf.tailWithProcessorOptimized(ctx, nil, reader,
			make(chan struct{}), rf.newFilteringProcessor(ltx, processor, re)); err != nil {
			t.Fatalf("tail returned error; max-count sentinel must be swallowed: %v", err)
		}
		return processor
	}

	t.Run("newline terminated line site", func(t *testing.T) {
		rf := newReadFile(defaultMaxLineLength)
		// The second complete (newline-terminated) match hits the count and stops
		// via the newline branch's processPartialLine call.
		p := runTail(t, context.Background(), &rf, "match1\nmatch2\nmatch3\n",
			lcontext.LContext{MaxCount: 2}, mustRegex("match"))
		want := []string{"match1", "match2"}
		if !reflect.DeepEqual(p.lines, want) {
			t.Fatalf("lines: got=%v want=%v", p.lines, want)
		}
	})

	t.Run("long line split site", func(t *testing.T) {
		rf := newReadFile(4)
		// "aa\n" reaches count 1 via the newline branch; the un-terminated 6-byte
		// "aaaaaa" exceeds the 4-byte line limit and is flushed by the long-line
		// branch, reaching count 2 (max) at that site.
		p := runTail(t, context.Background(), &rf, "aa\naaaaaa",
			lcontext.LContext{MaxCount: 2}, mustRegex("a"))
		want := []string{"aa", "aaaaaa"}
		if !reflect.DeepEqual(p.lines, want) {
			t.Fatalf("lines: got=%v want=%v", p.lines, want)
		}
	})

	t.Run("context cancel trailing partial site", func(t *testing.T) {
		rf := newReadFile(defaultMaxLineLength)
		// The final "match2" has no trailing newline, so it stays buffered as a
		// partial line. A pre-canceled context routes it through the ctx.Done
		// cleanup branch, where it reaches count 2 (max). The 64KB read buffer
		// consumes the whole 13-byte input in one Read (err==nil), so the loop
		// reaches the bottom ctx.Done select with the partial line still pending.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := runTail(t, ctx, &rf, "match1\nmatch2",
			lcontext.LContext{MaxCount: 2}, mustRegex("match"))
		want := []string{"match1", "match2"}
		if !reflect.DeepEqual(p.lines, want) {
			t.Fatalf("lines: got=%v want=%v", p.lines, want)
		}
	})
}

// nonRecyclingErrorProcessor returns an error WITHOUT recycling the buffer, so a
// test can observe whether filteringProcessor wrongly recycles a buffer whose
// ownership it already transferred to the processor. RecycleBytesBuffer calls
// buf.Reset(), so a stray recycle on the error path clears the payload - which
// this processor's caller can then detect.
type nonRecyclingErrorProcessor struct {
	err error
}

func (p nonRecyclingErrorProcessor) ProcessLine(_ *bytes.Buffer, _ uint64, _ string) error {
	return p.err
}

func (p nonRecyclingErrorProcessor) Flush() error { return nil }

func (p nonRecyclingErrorProcessor) Close() error { return nil }

// recyclingErrorProcessor mimics the real fs-path processors (DirectLineProcessor
// and AggregateProcessor): it recycles the buffer AND returns an error,
// exactly as DirectLineProcessor does when WriteLineData fails on a client
// disconnect / broken pipe. If filteringProcessor also recycled on error, the
// same buffer would be Put into the shared pool twice.
type recyclingErrorProcessor struct {
	err error
}

func (p recyclingErrorProcessor) ProcessLine(b *bytes.Buffer, _ uint64, _ string) error {
	pool.RecycleBytesBuffer(b)
	return p.err
}

func (p recyclingErrorProcessor) Flush() error { return nil }

func (p recyclingErrorProcessor) Close() error { return nil }

type panickingContextProcessor struct {
	transferred *bytes.Buffer
}

func (p *panickingContextProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	p.transferred = buf
	panic("before-context processor panic")
}

func (p *panickingContextProcessor) Flush() error { return nil }

func (p *panickingContextProcessor) Close() error { return nil }

type recyclingPanickingProcessor struct {
	recycle     func(*bytes.Buffer)
	transferred *bytes.Buffer
}

func (p *recyclingPanickingProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	p.transferred = buf
	p.recycle(buf)
	panic("reader processor panic")
}

func (p *recyclingPanickingProcessor) Flush() error { return nil }

func (p *recyclingPanickingProcessor) Close() error { return nil }

func TestReadWithProcessorOptimizedTransfersCurrentBufferBeforeContextPanic(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		maxLineLength int
		pattern       string
	}{
		{
			name:          "newline",
			input:         "old before\nMATCH\n",
			maxLineLength: defaultMaxLineLength,
			pattern:       "MATCH",
		},
		{
			name:          "forced long line",
			input:         "x\nMATC",
			maxLineLength: 4,
			pattern:       "MATC",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			re, err := regex.New(test.pattern, regex.Default)
			if err != nil {
				t.Fatalf("compile regex: %v", err)
			}

			rf := ReadFile{
				logger:        testLogger,
				filePath:      "memory.log",
				globID:        "glob-id",
				maxLineLength: test.maxLineLength,
			}
			recycleCount := make(map[*bytes.Buffer]int)
			rf.bufferRecycleObserver = func(buf *bytes.Buffer) {
				recycleCount[buf]++
			}
			processor := &recyclingPanickingProcessor{recycle: rf.recycleBytesBuffer}

			var recovered any
			func() {
				defer func() {
					recovered = recover()
				}()
				_ = rf.readWithProcessorOptimized(
					context.Background(),
					nil,
					bufio.NewReader(strings.NewReader(test.input)),
					nil,
					rf.newFilteringProcessor(lcontext.LContext{BeforeContext: 1}, processor, re),
				)
			}()

			if recovered != "reader processor panic" {
				t.Fatalf("recovered panic = %v, want reader processor panic", recovered)
			}
			if processor.transferred == nil {
				t.Fatal("processor did not receive buffered before-context")
			}
			if got := recycleCount[processor.transferred]; got != 1 {
				t.Fatalf("transferred before-context recycle count = %d, want 1", got)
			}
			if len(recycleCount) != 2 {
				t.Fatalf("distinct recycled buffers = %d, want before-context and current line", len(recycleCount))
			}
			for buf, count := range recycleCount {
				if count != 1 {
					t.Fatalf("buffer %p recycle count = %d, want exactly 1", buf, count)
				}
			}
		})
	}
}

func TestFilteringProcessorRecyclesOwnedBuffersOnBeforeContextPanic(t *testing.T) {
	transferredBefore := pool.BytesBuffer.Get().(*bytes.Buffer)
	transferredBefore.Reset()
	transferredBefore.WriteString("first before")
	// Ownership transfers to the panicking processor, which deliberately does
	// not recycle its input so the test can verify the filtering processor did
	// not reclaim a transferred buffer.
	defer pool.RecycleBytesBuffer(transferredBefore)

	remainingBefore := pool.BytesBuffer.Get().(*bytes.Buffer)
	remainingBefore.Reset()
	remainingBefore.WriteString("second before")
	matchingRaw := pool.BytesBuffer.Get().(*bytes.Buffer)
	matchingRaw.Reset()
	matchingRaw.WriteString("matching line")

	processor := &panickingContextProcessor{}
	var st stats
	fp := &filteringProcessor{
		processor: processor,
		re:        regex.NewNoop(),
		ltx:       lcontext.LContext{BeforeContext: 2},
		stats:     &st,
		globID:    "glob-id",
		beforeBuf: []*bytes.Buffer{transferredBefore, remainingBefore},
	}

	var recovered any
	func() {
		// Reader entry points own generation cleanup. Mimic that scope here so
		// the panic test covers both the current raw line and queued context.
		defer fp.resetGeneration()
		defer func() {
			recovered = recover()
		}()
		_ = fp.processWithContext(matchingRaw, 3)
	}()

	if recovered != "before-context processor panic" {
		t.Fatalf("recovered panic = %v, want before-context processor panic", recovered)
	}
	if processor.transferred != transferredBefore {
		t.Fatalf("processor received buffer %p, want first before-context buffer %p",
			processor.transferred, transferredBefore)
	}
	if got := transferredBefore.String(); got != "first before" {
		t.Fatalf("transferred buffer was recycled: got %q", got)
	}
	if matchingRaw.Len() != 0 {
		t.Fatalf("locally owned matching buffer length = %d, want 0 after recycle", matchingRaw.Len())
	}
	if remainingBefore.Len() != 0 {
		t.Fatalf("locally owned before-context buffer length = %d, want 0 after cleanup", remainingBefore.Len())
	}
	if len(fp.beforeBuf) != 0 {
		t.Fatalf("before-context buffers = %d, want 0 after cleanup", len(fp.beforeBuf))
	}
}

// TestFilteringProcessorDoesNotDoubleRecycleOnError is the regression guard for
// the yu0 production data race on the FILE read path (dcat/dgrep/dtail), the same
// class of bug fixed for the journal path in bt0 (1fe127a). The line.Processor
// contract transfers rawLine ownership to the processor, which recycles it on
// every return path (DirectLineProcessor and AggregateProcessor recycle
// unconditionally, even when ProcessLine returns a write error). If
// filteringProcessor also recycled on error, the same buffer would be returned to
// the shared pool.BytesBuffer twice; the pool would then hand one object to two
// Get callers whose concurrent writes race and corrupt data.
//
// The two caller-buffer error sites (processWithContext after-context and
// matched-line sites) are checked with a
// nonRecyclingErrorProcessor: post-fix the buffer must be left untouched on error
// (payload survives). Pre-fix filteringProcessor called RecycleBytesBuffer -> the
// buffer was Reset and the payload vanished, so each sub-case goes red.
func TestFilteringProcessorDoesNotDoubleRecycleOnError(t *testing.T) {
	sinkErr := errors.New("processor stopped")
	matchAll := regex.NewNoop()
	noMatch, err := regex.New("NEEDLE_THAT_NEVER_MATCHES", regex.Default)
	if err != nil {
		t.Fatalf("build regex: %v", err)
	}

	tests := []struct {
		name    string
		ltx     lcontext.LContext
		re      regex.Regex
		payload string
		// primeAfter installs a pending after-context window so a non-matching line
		// is emitted through the after-context ProcessLine site.
		primeAfter bool
	}{
		{
			name:       "context after-context site",
			ltx:        lcontext.LContext{BeforeContext: 1, AfterContext: 1},
			re:         noMatch,
			payload:    "after-context-payload",
			primeAfter: true,
		},
		{
			name:    "context matched-line site",
			ltx:     lcontext.LContext{BeforeContext: 1, AfterContext: 1},
			re:      matchAll,
			payload: "matched-line-payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st stats
			fp := &filteringProcessor{
				processor: nonRecyclingErrorProcessor{err: sinkErr},
				re:        tt.re,
				ltx:       tt.ltx,
				stats:     &st,
				globID:    "glob-id",
			}
			if tt.primeAfter {
				fp.afterCount = 1
			}

			buf := pool.BytesBuffer.Get().(*bytes.Buffer)
			buf.Reset()
			buf.WriteString(tt.payload)

			if err := fp.processWithContext(buf, 1); !errors.Is(err, sinkErr) {
				t.Fatalf("processWithContext error = %v, want %v", err, sinkErr)
			}
			if got := buf.String(); got != tt.payload {
				t.Fatalf("filteringProcessor recycled a buffer it does not own "+
					"(double-recycle regression): buf=%q want=%q", got, tt.payload)
			}

			// The processor did not recycle (test double), so recycle here to avoid
			// leaking the pooled buffer out of the test.
			pool.RecycleBytesBuffer(buf)
		})
	}
}

// TestProcessFilteredRawDoesNotDoubleRecycleOnError guards the no-context error site,
// the zero-copy fast path ProcessFilteredRaw, which acquires its own pooled buffer
// internally (so payload survival cannot be observed from outside). It uses a
// recyclingErrorProcessor that faithfully mimics DirectLineProcessor - recycle the
// buffer, then return a write error. Pre-fix, ProcessFilteredRaw recycled the same
// buffer a second time, Putting one pointer into the pool twice; a subsequent
// sweep of the pool then hands out that pointer more than once. Post-fix the
// buffer is Put exactly once and no duplicate appears.
func TestProcessFilteredRawDoesNotDoubleRecycleOnError(t *testing.T) {
	drainBytesBufferPool()

	sinkErr := errors.New("processor stopped")
	var st stats
	fp := &filteringProcessor{
		processor: recyclingErrorProcessor{err: sinkErr},
		re:        regex.NewNoop(),
		ltx:       lcontext.LContext{},
		stats:     &st,
		globID:    "glob-id",
	}

	if err := fp.ProcessFilteredRaw([]byte("match me\n")); !errors.Is(err, sinkErr) {
		t.Fatalf("ProcessFilteredRaw error = %v, want %v", err, sinkErr)
	}

	seen := make(map[*bytes.Buffer]int)
	for i := 0; i < 512; i++ {
		b := pool.BytesBuffer.Get().(*bytes.Buffer)
		seen[b]++
		if seen[b] > 1 {
			t.Fatalf("buffer %p observed in pool more than once: double-recycle "+
				"detected (Put twice into sync.Pool) on ProcessFilteredRaw error path", b)
		}
	}
}

func writeProcessorTestFile(t *testing.T, content string) string {
	t.Helper()

	filePath := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
		t.Fatalf("unable to write test file: %v", err)
	}
	return filePath
}
