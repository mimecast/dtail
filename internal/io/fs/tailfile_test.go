package fs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

type tailCaptureProcessor struct {
	lines chan string
}

type readCycleObserver struct {
	reader        io.Reader
	cycleComplete chan struct{}
	pending       bool
}

func (p *tailCaptureProcessor) ProcessLine(content *bytes.Buffer, _ uint64, _ string) error {
	line := content.String()
	pool.RecycleBytesBuffer(content)
	p.lines <- line
	return nil
}

func (*tailCaptureProcessor) Flush() error { return nil }
func (*tailCaptureProcessor) Close() error { return nil }

func (r *readCycleObserver) Read(data []byte) (int, error) {
	if r.pending {
		r.pending = false
		r.cycleComplete <- struct{}{}
	}
	n, err := r.reader.Read(data)
	if n > 0 {
		r.pending = true
	}
	return n, err
}

func TestTailFileFollowsCopytruncateAndRenameRotations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "application.log")
	writeTestPath(t, path, "historical\n")

	tail := newFollowReadFile(path, "application.log", nil, defaultMaxLineLength, testLogger)
	opened := make(chan struct{}, 4)
	var checkerStarts atomic.Int32
	var checkerStops atomic.Int32
	tail.truncateCheck = func(ctx context.Context, _ chan<- struct{}) {
		checkerStarts.Add(1)
		select {
		case opened <- struct{}{}:
		case <-ctx.Done():
			checkerStops.Add(1)
			return
		}
		<-ctx.Done()
		checkerStops.Add(1)
	}

	processor := &tailCaptureProcessor{lines: make(chan string, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startAttempt := func() <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			done <- tail.Start(
				ctx,
				lcontext.LContext{},
				processor,
				regex.NewNoop(),
			)
		}()
		select {
		case <-opened:
		case <-time.After(2 * time.Second):
			t.Fatal("tail did not finish opening the file")
		}
		return done
	}

	firstAttempt := startAttempt()
	assertNoTailLine(t, processor.lines)

	appendTestPath(t, path, "live-before-copytruncate\n")
	wantLines := []string{"live-before-copytruncate"}
	assertTailLine(t, processor.lines, wantLines[len(wantLines)-1])

	// Write immediately after truncating, before the tail has observed the
	// shrink. The current descriptor must rewind to byte zero so this line is
	// delivered without waiting for the server's outer retry loop.
	writeTestPath(t, path, "immediate-after-copytruncate\n")
	wantLines = append(wantLines, "immediate-after-copytruncate")
	assertTailLine(t, processor.lines, wantLines[len(wantLines)-1])
	appendTestPath(t, path, "live-before-rename\n")
	wantLines = append(wantLines, "live-before-rename")
	assertTailLine(t, processor.lines, wantLines[len(wantLines)-1])

	rotatedPath := path + ".1"
	// This replacement is deliberately larger than the old descriptor offset.
	// A size-only check would miss the rotation; descriptor identity must catch it.
	replacementLine := "immediate-after-rename-" + strings.Repeat("x", 80)
	replaceTestPath(t, path, rotatedPath, replacementLine+"\n")
	assertFileChange(t, firstAttempt, "rotated")

	secondAttempt := startAttempt()
	wantLines = append(wantLines, replacementLine)
	assertTailLine(t, processor.lines, wantLines[len(wantLines)-1])
	appendTestPath(t, path, "live-before-second-rename\n")
	wantLines = append(wantLines, "live-before-second-rename")
	assertTailLine(t, processor.lines, wantLines[len(wantLines)-1])

	replaceTestPath(t, path, path+".2", "immediate-after-second-rename\n")
	assertFileChange(t, secondAttempt, "rotated")

	thirdAttempt := startAttempt()
	wantLines = append(wantLines, "immediate-after-second-rename")
	assertTailLine(t, processor.lines, wantLines[len(wantLines)-1])

	cancel()
	select {
	case err := <-thirdAttempt:
		if err != nil {
			t.Fatalf("canceled tail returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not stop after cancellation")
	}

	if got, want := checkerStops.Load(), checkerStarts.Load(); got != want {
		t.Fatalf("truncate-check workers stopped = %d, want %d", got, want)
	}
	select {
	case duplicate := <-processor.lines:
		t.Fatalf("unexpected duplicate or historical line: %q; expected sequence %v", duplicate, wantLines)
	default:
	}
}

func TestTailFileRotationGapPreservesReopenFromStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application.log")
	writeTestPath(t, path, "historical\n")
	tail := newFollowReadFile(path, "application.log", nil, defaultMaxLineLength, testLogger)

	initialReader, initialFD, initialDecompressor, err := tail.makeReader(context.Background())
	if err != nil {
		t.Fatalf("open initial tail: %v", err)
	}
	if initialDecompressor != nil {
		t.Fatal("plain tail unexpectedly returned a decompressor")
	}
	initialData, err := io.ReadAll(initialReader)
	if err != nil {
		t.Fatalf("read initial tail: %v", err)
	}
	if len(initialData) != 0 {
		t.Fatalf("initial tail data = %q, want historical data skipped", initialData)
	}
	if closeErr := initialFD.Close(); closeErr != nil {
		t.Fatalf("close initial tail: %v", closeErr)
	}

	if renameErr := os.Rename(path, path+".1"); renameErr != nil {
		t.Fatalf("rename followed file: %v", renameErr)
	}
	_, missingFD, missingDecompressor, missingErr := tail.makeReader(context.Background())
	if missingDecompressor != nil {
		_ = missingDecompressor.Close()
	}
	if missingFD != nil {
		_ = missingFD.Close()
	}
	if missingErr == nil {
		t.Fatal("reopen unexpectedly succeeded while the followed path was absent")
	}

	writeTestPath(t, path, "immediate-after-gap\n")
	reopenedReader, reopenedFD, reopenedDecompressor, err := tail.makeReader(context.Background())
	if err != nil {
		t.Fatalf("reopen recreated tail: %v", err)
	}
	if reopenedDecompressor != nil {
		t.Fatal("plain reopened tail unexpectedly returned a decompressor")
	}
	reopenedData, err := io.ReadAll(reopenedReader)
	if err != nil {
		t.Fatalf("read recreated tail: %v", err)
	}
	if got, want := string(reopenedData), "immediate-after-gap\n"; got != want {
		t.Fatalf("recreated tail data = %q, want %q", got, want)
	}
	if err := reopenedFD.Close(); err != nil {
		t.Fatalf("close recreated tail: %v", err)
	}
}

func TestTailFileCopytruncateResetsLocalContext(t *testing.T) {
	tests := []struct {
		name           string
		context        lcontext.LContext
		oldData        string
		oldOutput      string
		replacement    string
		firstNewOutput string
	}{
		{
			name:           "before context",
			context:        lcontext.LContext{BeforeContext: 1},
			oldData:        "old-nonmatch\n",
			replacement:    "MATCH-new\n",
			firstNewOutput: "MATCH-new",
		},
		{
			name:           "after context",
			context:        lcontext.LContext{AfterContext: 1},
			oldData:        "MATCH-old\n",
			oldOutput:      "MATCH-old",
			replacement:    "new-nonmatch\nMATCH-new\n",
			firstNewOutput: "MATCH-new",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runCopytruncateContextBoundaryTest(t, test.context, test.oldData,
				test.oldOutput, test.replacement, test.firstNewOutput)
		})
	}
}

func TestFilteringProcessorResetGeneration(t *testing.T) {
	lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
	lineBuf.WriteString("old generation")
	fp := filteringProcessor{
		beforeBuf:  []*bytes.Buffer{lineBuf},
		afterCount: 2,
		maxCount:   3,
		maxReached: true,
	}

	fp.resetGeneration()

	if len(fp.beforeBuf) != 0 {
		t.Fatalf("before-context buffers = %d, want 0", len(fp.beforeBuf))
	}
	if lineBuf.Len() != 0 {
		t.Fatalf("recycled before-context buffer length = %d, want 0", lineBuf.Len())
	}
	if fp.afterCount != 0 {
		t.Fatalf("after-context count = %d, want 0", fp.afterCount)
	}
	if fp.maxCount != 3 || !fp.maxReached {
		t.Fatalf("query-wide max state changed: count=%d reached=%v", fp.maxCount, fp.maxReached)
	}
}

func TestTailFileCopytruncateResetsLongLineWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long-line.log")
	writeTestPath(t, path, "historical\n")
	warnings := make(chan string, 2)
	tail := newFollowReadFile(path, "long-line.log", warnings, 4, testLogger)
	_, fd, decompressor, err := tail.makeReader(context.Background())
	if err != nil {
		t.Fatalf("open long-line tail: %v", err)
	}
	if decompressor != nil {
		t.Fatal("plain long-line tail unexpectedly returned a decompressor")
	}

	cycleComplete := make(chan struct{}, 1)
	observedReader := &readCycleObserver{reader: fd, cycleComplete: cycleComplete}
	processor := &tailCaptureProcessor{lines: make(chan string, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tail.tailWithProcessorOptimized(ctx, fd, bufio.NewReader(observedReader), nil,
			tail.newFilteringProcessor(lcontext.LContext{}, processor, regex.NewNoop()))
	}()
	defer stopObservedTail(t, cancel, done, fd)

	appendTestPath(t, path, "abcde")
	assertTailLine(t, processor.lines, "abcde")
	assertLongLineWarning(t, warnings)
	select {
	case <-cycleComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not finish processing old-generation long line")
	}

	// Neither generation has a newline. The copytruncate boundary itself must
	// end the old long-line sequence so the new generation warns independently.
	writeTestPath(t, path, "vwxyz")
	assertTailLine(t, processor.lines, "vwxyz")
	assertLongLineWarning(t, warnings)
}

func TestNonFollowReadersStillStartAtBeginningOnEveryOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.log")
	writeTestPath(t, path, "alpha\nbeta\n")
	cat := newSnapshotReadFile(path, "snapshot.log", nil, defaultMaxLineLength, testLogger)

	for attempt := 1; attempt <= 2; attempt++ {
		processor := &captureProcessor{}
		if err := cat.Start(
			context.Background(),
			lcontext.LContext{},
			processor,
			regex.NewNoop(),
		); err != nil {
			t.Fatalf("snapshot read %d: %v", attempt, err)
		}
		want := []string{"alpha\n", "beta\n"}
		if !reflect.DeepEqual(processor.lines, want) {
			t.Fatalf("snapshot read %d lines = %q, want %q", attempt, processor.lines, want)
		}
	}
}

func TestCompressedNonFollowReaderUnaffected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.log.gz")
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("create gzip snapshot: %v", err)
	}
	gzipWriter := gzip.NewWriter(fd)
	if _, err := gzipWriter.Write([]byte("compressed-alpha\ncompressed-beta\n")); err != nil {
		_ = gzipWriter.Close()
		_ = fd.Close()
		t.Fatalf("write gzip snapshot: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		_ = fd.Close()
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := fd.Close(); err != nil {
		t.Fatalf("close gzip snapshot: %v", err)
	}

	cat := newSnapshotReadFile(path, "snapshot.log.gz", nil, defaultMaxLineLength, testLogger)
	processor := &captureProcessor{}
	if err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		regex.NewNoop(),
	); err != nil {
		t.Fatalf("read gzip snapshot: %v", err)
	}
	want := []string{"compressed-alpha\n", "compressed-beta\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("gzip snapshot lines = %q, want %q", processor.lines, want)
	}
}

func assertFileChange(t *testing.T, done <-chan error, want string) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("file-change error = %v, want error containing %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("tail did not detect file change containing %q", want)
	}
}

// restartRecordingProcessor is a line.SourceRestarter that reports every
// line and every SourceRestarted call on one channel, in call order.
type restartRecordingProcessor struct {
	events chan string
}

func (p *restartRecordingProcessor) ProcessLine(content *bytes.Buffer, _ uint64, _ string) error {
	event := "line " + content.String()
	pool.RecycleBytesBuffer(content)
	p.events <- event
	return nil
}

func (p *restartRecordingProcessor) SourceRestarted() { p.events <- "restart" }
func (*restartRecordingProcessor) Flush() error       { return nil }
func (*restartRecordingProcessor) Close() error       { return nil }

// TestTailFileCopytruncateRestartsSource pins the line.SourceRestarter
// contract of the follow reader: an in-place truncation is reported once,
// after the old content's last line and before the new content's first line,
// and ending the read does not report one.
func TestTailFileCopytruncateRestartsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.log")
	writeTestPath(t, path, strings.Repeat("historical-data", 16)+"\n")
	tail := newFollowReadFile(path, "restart.log", nil, defaultMaxLineLength, testLogger)
	_, fd, decompressor, err := tail.makeReader(context.Background())
	if err != nil {
		t.Fatalf("open restart tail: %v", err)
	}
	if decompressor != nil {
		t.Fatal("plain restart tail unexpectedly returned a decompressor")
	}

	cycleComplete := make(chan struct{}, 1)
	observedReader := &readCycleObserver{reader: fd, cycleComplete: cycleComplete}
	processor := &restartRecordingProcessor{events: make(chan string, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tail.tailWithProcessorOptimized(ctx, fd, bufio.NewReader(observedReader), nil,
			tail.newFilteringProcessor(lcontext.LContext{}, processor, regex.NewNoop()))
	}()

	appendTestPath(t, path, "old-1\nold-2\n")
	assertTailLine(t, processor.events, "line old-1")
	assertTailLine(t, processor.events, "line old-2")
	select {
	case <-cycleComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not finish processing the old content")
	}

	writeTestPath(t, path, "new-1\n")
	assertTailLine(t, processor.events, "restart")
	assertTailLine(t, processor.events, "line new-1")

	stopObservedTail(t, cancel, done, fd)
	assertNoTailLine(t, processor.events)
}

func runCopytruncateContextBoundaryTest(t *testing.T, localContext lcontext.LContext,
	oldData, oldOutput, replacement, firstNewOutput string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "context.log")
	writeTestPath(t, path, strings.Repeat("historical-data", 16)+"\n")
	tail := newFollowReadFile(path, "context.log", nil, defaultMaxLineLength, testLogger)
	_, fd, decompressor, err := tail.makeReader(context.Background())
	if err != nil {
		t.Fatalf("open context tail: %v", err)
	}
	if decompressor != nil {
		t.Fatal("plain context tail unexpectedly returned a decompressor")
	}

	cycleComplete := make(chan struct{}, 1)
	observedReader := &readCycleObserver{reader: fd, cycleComplete: cycleComplete}
	processor := &tailCaptureProcessor{lines: make(chan string, 8)}
	re, err := regex.New("MATCH", regex.Default)
	if err != nil {
		_ = fd.Close()
		t.Fatalf("compile context regex: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tail.tailWithProcessorOptimized(ctx, fd, bufio.NewReader(observedReader), nil,
			tail.newFilteringProcessor(localContext, processor, re))
	}()
	defer stopObservedTail(t, cancel, done, fd)

	appendTestPath(t, path, oldData)
	if oldOutput != "" {
		assertTailLine(t, processor.lines, oldOutput)
	}
	select {
	case <-cycleComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not finish processing old-generation context")
	}

	writeTestPath(t, path, replacement)
	assertTailLine(t, processor.lines, firstNewOutput)
	assertNoTailLine(t, processor.lines)
}

func stopObservedTail(t *testing.T, cancel context.CancelFunc, done <-chan error, fd *os.File) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stop context tail: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("context tail did not stop after cancellation")
	}
	if err := fd.Close(); err != nil {
		t.Errorf("close context tail: %v", err)
	}
}

func assertTailLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got := <-lines:
		if got != want {
			t.Fatalf("tail line = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("tail did not deliver %q", want)
	}
}

func assertNoTailLine(t *testing.T, lines <-chan string) {
	t.Helper()
	select {
	case got := <-lines:
		t.Fatalf("tail delivered unexpected line %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func assertLongLineWarning(t *testing.T, warnings <-chan string) {
	t.Helper()
	select {
	case <-warnings:
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not emit long-line warning")
	}
}

func writeTestPath(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendTestPath(t *testing.T, path, content string) {
	t.Helper()
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	if _, err := fd.WriteString(content); err != nil {
		_ = fd.Close()
		t.Fatalf("append %s: %v", path, err)
	}
	if err := fd.Close(); err != nil {
		t.Fatalf("close appended %s: %v", path, err)
	}
}

func replaceTestPath(t *testing.T, path, rotatedPath, content string) {
	t.Helper()

	replacementPath := path + ".replacement"
	writeTestPath(t, replacementPath, content)
	if err := os.Link(path, rotatedPath); err != nil {
		t.Fatalf("link rotated file %s: %v", rotatedPath, err)
	}
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatalf("replace followed file %s: %v", path, err)
	}
}
