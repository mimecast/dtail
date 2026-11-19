package fs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

const mib = 1024 * 1024

// longLineCase is a snapshot read of content with maxLineLength (0 for the
// default) and the lines and long-line warnings it must produce.
type longLineCase struct {
	name          string
	content       string
	maxLineLength int
	want          []string
	// wantWarnings is how many long-line warnings the reader sends: none
	// when every line arrives whole, one for a file with split lines (the
	// reader warns once per sequence of split chunks).
	wantWarnings int
}

// longLineCases covers lines longer than the pooled 1 MiB scanner buffer:
// below or at MaxLineLength such a line arrives whole, above it the reader
// splits it at MaxLineLength, and in every case the read goes on with the next
// line instead of failing with bufio.Scanner's ErrTooLong. The exact-limit
// cases at 1 and 2 MiB are on bufio.Scanner's doubling sequence from the
// pooled buffer, where the buffer fills at exactly the limit.
func longLineCases() []longLineCase {
	x := func(n int) string { return strings.Repeat("x", n) }
	whole := func(name string, limit int, line string) longLineCase {
		return longLineCase{name: name, content: "a\n" + line + "\nb\n", maxLineLength: limit,
			want: []string{"a\n", line + "\n", "b\n"}}
	}
	split := func(name string, limit, cut int, line string) longLineCase {
		return longLineCase{name: name, content: "a\n" + line + "\nb\n", maxLineLength: limit,
			want: []string{"a\n", line[:cut], line[cut:] + "\n", "b\n"}, wantWarnings: 1}
	}
	final := func(name string, limit int, line string, want ...string) longLineCase {
		c := longLineCase{name: name, content: "a\n" + line, maxLineLength: limit,
			want: append([]string{"a\n"}, want...)}
		if len(want) > 1 {
			c.wantWarnings = 1
		}
		return c
	}
	long := x(mib + mib/2)
	crlf := x(mib+mib/2) + "\r"
	return []longLineCase{
		whole("line below MaxLineLength above 1 MiB arrives whole", 4*mib, long),
		whole("line of exactly MaxLineLength off the doubling sequence", len(long), long),
		whole("line of exactly the default MaxLineLength", 0, x(mib)),
		whole("line of exactly MaxLineLength of 2 MiB", 2*mib, x(2*mib)),
		split("line one byte above MaxLineLength of 2 MiB", 2*mib, 2*mib, x(2*mib+1)),
		split("line above MaxLineLength above 1 MiB is split", mib+mib/4, mib+mib/4, long),
		split("default MaxLineLength splits at 1 MiB", 0, mib, long),
		whole("CRLF line below MaxLineLength above 1 MiB arrives whole", 4*mib, crlf),
		// The carriage return is part of the line before its newline.
		whole("CRLF line of exactly MaxLineLength of 2 MiB", 2*mib, x(2*mib-1)+"\r"),
		final("final line without newline below MaxLineLength", 4*mib, long, long),
		final("final line without newline of exactly MaxLineLength", 2*mib, x(2*mib), x(2*mib)),
		final("final line without newline above MaxLineLength", mib+mib/4, long,
			long[:mib+mib/4], long[mib+mib/4:]),
	}
}

func TestStartReadsLinesLongerThanScannerBuffer(t *testing.T) {
	for _, tt := range longLineCases() {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProcessorTestFile(t, tt.content)
			assertLongLineRead(t, path, tt)
		})
	}
}

// TestStartReadsLongLinesOfGzipFiles runs the long-line cases through the
// gzip decompressor, whose reader hands the scanner data in other chunks.
func TestStartReadsLongLinesOfGzipFiles(t *testing.T) {
	for _, tt := range longLineCases() {
		t.Run(tt.name, func(t *testing.T) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write([]byte(tt.content)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			assertLongLineRead(t, writeLongLineFile(t, "test.log.gz", compressed.Bytes()), tt)
		})
	}
}

// writeLongLineFile writes content to a file name in a new temporary
// directory and returns its path.
func writeLongLineFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertLongLineRead reads path with a snapshot reader and checks the lines,
// their numbers and the long-line warnings against tt.
func assertLongLineRead(t *testing.T, path string, tt longLineCase) {
	t.Helper()
	messages := make(chan string, 10)
	cat := newSnapshotReadFile(path, "glob-id", messages, tt.maxLineLength, testLogger)
	processor := &captureProcessor{}

	if err := cat.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("reader start failed: %v", err)
	}
	assertLinesEqual(t, processor.lines, tt.want)
	for i, lineNum := range processor.lineNums {
		if lineNum != uint64(i+1) {
			t.Fatalf("line %d has number %d, want %d", i, lineNum, i+1)
		}
	}
	assertLongLineWarnings(t, messages, tt.wantWarnings)
}

// assertLongLineWarnings checks that messages holds exactly want server
// messages. A reader of these tests sends no other message than the long-line
// warning. The test logger logs nothing, like a logger whose level filters
// warnings out, so its text is the fallback wording of logging.WarnMessage,
// which still tells the client of the split line.
func assertLongLineWarnings(t *testing.T, messages chan string, want int) {
	t.Helper()
	close(messages)
	got := 0
	for message := range messages {
		if !strings.HasPrefix(message, "WARN|") ||
			!strings.HasSuffix(message, "|Long log line, splitting into multiple lines\n") {
			t.Fatalf("unexpected server message %q", message)
		}
		got++
	}
	if got != want {
		t.Fatalf("got %d long-line warnings, want %d", got, want)
	}
}

// chunkReader returns one chunk per Read, so a test decides where the reads of
// a follow reader end, and cancels the read on the Read after the last chunk,
// so that no long-line warning of the last chunk races the cancellation.
type chunkReader struct {
	chunks []string
	cancel context.CancelFunc
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		r.cancel()
		return 0, nil
	}
	n := copy(p, r.chunks[0])
	if r.chunks[0] = r.chunks[0][n:]; r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

// lineCutCase is content that a follow read gets in the reads chunks and a
// snapshot read gets at once, with MaxLineLength 4, and the lines both feed:
// terminators included, as a snapshot read and a follow read whose processor
// preserves terminators feed them. Empty lines are left out, as only a
// snapshot read feeds them.
type lineCutCase struct {
	name         string
	chunks       []string
	want         []string
	wantWarnings int
}

// lineCutCases covers the cuts of lines longer than MaxLineLength 4: at
// exactly the limit, whatever the reads a line arrives in, with a line of
// exactly the limit kept whole, also as the final line without newline (a
// follow read feeds it when canceled, see followLineProcessor.finish).
func lineCutCases() []lineCutCase {
	bytewise := func(s string) []string { return strings.Split(s, "") }
	return []lineCutCase{
		{name: "exact limit, newline in the next read", chunks: []string{"abcd", "\nxy\n"},
			want: []string{"abcd\n", "xy\n"}},
		{name: "exact limit over several reads", chunks: []string{"ab", "cd", "\n"}, want: []string{"abcd\n"}},
		{name: "exact limit in one read", chunks: []string{"abcd\nxy\n"}, want: []string{"abcd\n", "xy\n"}},
		{name: "one byte above the limit", chunks: []string{"abcde", "\nxy\n"},
			want: []string{"abcd", "e\n", "xy\n"}, wantWarnings: 1},
		{name: "above the limit over several reads", chunks: []string{"abcd", "e", "fg\n"},
			want: []string{"abcd", "efg\n"}, wantWarnings: 1},
		{name: "above the limit with its newline in one read", chunks: []string{"abcdefgh\nxy\n"},
			want: []string{"abcd", "efgh\n", "xy\n"}, wantWarnings: 1},
		{name: "newline in the read after a pending start", chunks: []string{"abc", "defgh\n"},
			want: []string{"abcd", "efgh\n"}, wantWarnings: 1},
		{name: "byte by byte", chunks: bytewise("abcdef\nxy\n"),
			want: []string{"abcd", "ef\n", "xy\n"}, wantWarnings: 1},
		{name: "several pieces", chunks: []string{"abcdefghij\n"},
			want: []string{"abcd", "efgh", "ij\n"}, wantWarnings: 1},
		{name: "every long line warns", chunks: []string{"abcdef\nxyz12345\n"},
			want: []string{"abcd", "ef\n", "xyz1", "2345\n"}, wantWarnings: 2},
		{name: "CRLF after exactly the limit", chunks: []string{"abcd\r\n"},
			want: []string{"abcd", "\r\n"}, wantWarnings: 1},
		{name: "final line of exactly the limit", chunks: []string{"xy\nab", "cd"},
			want: []string{"xy\n", "abcd"}},
		{name: "final line above the limit byte by byte", chunks: bytewise("abcdef"),
			want: []string{"abcd", "ef"}, wantWarnings: 1},
	}
}

// TestFollowCutsLinesLikeSnapshotRead checks that a follow read cuts lines
// longer than MaxLineLength into the same pieces, numbered the same, with the
// same warnings, as a snapshot read of the same content, whatever the reads
// the follow read gets the content in.
func TestFollowCutsLinesLikeSnapshotRead(t *testing.T) {
	for _, tt := range lineCutCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("snapshot", func(t *testing.T) {
				path := writeProcessorTestFile(t, strings.Join(tt.chunks, ""))
				assertLongLineRead(t, path, longLineCase{maxLineLength: 4, want: tt.want,
					wantWarnings: tt.wantWarnings})
			})
			for _, preserve := range []bool{true, false} {
				t.Run(fmt.Sprintf("follow/preserve=%v", preserve), func(t *testing.T) {
					want := tt.want
					if !preserve {
						want = trimNewlines(want)
					}
					assertFollowCuts(t, tt.chunks, preserve, want, tt.wantWarnings)
				})
			}
		})
	}
}

// assertFollowCuts follows chunks with MaxLineLength 4 and checks the lines,
// their numbers and the long-line warnings.
func assertFollowCuts(t *testing.T, chunks []string, preserve bool, want []string, wantWarnings int) {
	t.Helper()
	messages := make(chan string, 10)
	rf := ReadFile{logger: testLogger, filePath: "follow.log", globID: "glob-id",
		serverMessages: messages, follow: true, maxLineLength: 4}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &chunkReader{chunks: append([]string(nil), chunks...), cancel: cancel}
	processor := &terminatorCaptureProcessor{preserve: preserve}

	// fd is nil: the reads never return io.EOF, so the reader never inspects
	// the file, and the context ends it after the last chunk.
	if err := rf.tailWithProcessorOptimized(ctx, nil, bufio.NewReader(reader), make(chan struct{}),
		rf.newFilteringProcessor(lcontext.LContext{}, processor, regex.NewNoop())); err != nil {
		t.Fatalf("tail returned error: %v", err)
	}
	assertLinesEqual(t, processor.lines, want)
	for i, lineNum := range processor.lineNums {
		if lineNum != uint64(i+1) {
			t.Fatalf("line %d has number %d, want %d", i, lineNum, i+1)
		}
	}
	assertLongLineWarnings(t, messages, wantWarnings)
}

func trimNewlines(lines []string) []string {
	trimmed := make([]string, len(lines))
	for i, l := range lines {
		trimmed[i] = strings.TrimSuffix(l, "\n")
	}
	return trimmed
}

// discardRawProcessor counts the lines it gets through the borrowing
// line.RawProcessor path, which copies nothing, so that a test measures what
// the reader allocates.
type discardRawProcessor struct{ lines int }

func (p *discardRawProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	pool.RecycleBytesBuffer(buf)
	p.lines++
	return nil
}

func (p *discardRawProcessor) ProcessRawLine([]byte, uint64, string) error {
	p.lines++
	return nil
}

func (p *discardRawProcessor) Flush() error { return nil }
func (p *discardRawProcessor) Close() error { return nil }

// TestSnapshotReadWithDefaultLimitStaysInPooledBuffer checks that a snapshot
// read with the default MaxLineLength, whose scanner token limit is 1 MiB + 1
// byte, reads a line longer than 1 MiB in the pooled scanner buffer instead
// of growing a buffer of its own (about 1 MiB more per read).
func TestSnapshotReadWithDefaultLimitStaysInPooledBuffer(t *testing.T) {
	path := writeProcessorTestFile(t, "a\n"+strings.Repeat("x", mib+mib/2)+"\nb\n")
	read := func() {
		t.Helper()
		cat := newSnapshotReadFile(path, "glob-id", nil, 0, testLogger)
		processor := &discardRawProcessor{}
		if err := cat.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
			t.Fatalf("reader start failed: %v", err)
		}
		// "a", the long line cut at 1 MiB into two pieces, and "b".
		if processor.lines != 4 {
			t.Fatalf("read %d lines, want 4", processor.lines)
		}
	}
	read() // puts a scanner buffer into the pool

	// The race detector makes sync.Pool drop a quarter of the buffers put
	// back, so each read may allocate a new pooled buffer that often; a
	// buffer grown for the long line would cost every read about 1 MiB.
	const runs = 20
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		read()
	}
	runtime.ReadMemStats(&after)
	if perRead := (after.TotalAlloc - before.TotalAlloc) / runs; perRead > 3*mib/4 {
		t.Fatalf("a read allocated %d bytes on average, want at most %d", perRead, 3*mib/4)
	}
}

// TestScannerTokenLimit checks that the snapshot scanner may always hold a
// line of the configured limit plus its newline, never less than the pooled
// buffer, and does not overflow for the largest int.
func TestScannerTokenLimit(t *testing.T) {
	tests := []struct {
		maxLineLength int
		want          int
	}{
		{maxLineLength: 0, want: defaultMaxLineLength + 1},
		{maxLineLength: -1, want: defaultMaxLineLength + 1},
		{maxLineLength: 1024, want: minReadScannerTokenSize},
		{maxLineLength: minReadScannerTokenSize - 1, want: minReadScannerTokenSize},
		{maxLineLength: minReadScannerTokenSize, want: minReadScannerTokenSize + 1},
		{maxLineLength: 4 * minReadScannerTokenSize, want: 4*minReadScannerTokenSize + 1},
		{maxLineLength: math.MaxInt, want: math.MaxInt},
	}

	for _, tt := range tests {
		f := ReadFile{maxLineLength: tt.maxLineLength}
		if got := f.scannerTokenLimit(); got != tt.want {
			t.Errorf("scannerTokenLimit() with MaxLineLength %d = %d, want %d",
				tt.maxLineLength, got, tt.want)
		}
	}
}

// assertLinesEqual compares line slices by length and content, without
// printing the multi-MiB lines of a mismatch.
func assertLinesEqual(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %d lines (lengths %v), want %d (lengths %v)",
			len(got), lineLengths(got), len(want), lineLengths(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d differs: got length %d, want length %d", i, len(got[i]), len(want[i]))
		}
	}
}

func lineLengths(lines []string) []int {
	lengths := make([]int, len(lines))
	for i, l := range lines {
		lengths[i] = len(l)
	}
	return lengths
}
