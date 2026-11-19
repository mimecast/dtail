//go:build linux

package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

const mib = 1024 * 1024

// warnLogger formats warnings, so that a test can tell the long-line warning
// from other server messages.
type warnLogger struct{ logging.NopLogger }

func (warnLogger) Warn(args ...any) string { return "WARN|" + fmt.Sprint(args...) }

// journalSplitCase is the stdout of one journalctl run, read with
// maxLineLength (0 for the default), and the lines and long-line warnings
// it must produce.
type journalSplitCase struct {
	name          string
	stdout        string
	maxLineLength int
	want          []string
	wantWarnings  int
}

// journalSplitCases mirror the long-line cases of file reads
// (internal/io/fs/readfile_longline_test.go): an entry of up to MaxLineLength
// arrives whole, a longer one is cut at exactly MaxLineLength, and the read
// goes on with the next entry instead of failing with bufio.Scanner's
// ErrTooLong. Unlike a file read, an unterminated last entry is dropped.
func journalSplitCases() []journalSplitCase {
	x := func(n int) string { return strings.Repeat("x", n) }
	long := x(2*mib + 5)
	return []journalSplitCase{
		{name: "entry above 1 MiB with the default limit is cut", stdout: "a\n" + long + "\nb\n",
			want: []string{"a\n", long[:mib], long[mib : 2*mib], long[2*mib:] + "\n", "b\n"}, wantWarnings: 1},
		{name: "entry of exactly the default limit arrives whole", stdout: x(mib) + "\nb\n",
			want: []string{x(mib) + "\n", "b\n"}},
		{name: "entry one byte above the default limit", stdout: x(mib+1) + "\nb\n",
			want: []string{x(mib), "x\n", "b\n"}, wantWarnings: 1},
		{name: "entry below a limit above 1 MiB arrives whole", stdout: long + "\nb\n", maxLineLength: 3 * mib,
			want: []string{long + "\n", "b\n"}},
		{name: "entry of exactly a limit above 1 MiB arrives whole", stdout: long + "\n", maxLineLength: len(long),
			want: []string{long + "\n"}},
		{name: "small limit cuts at exactly the limit", stdout: "abcd\nabcdefghij\nz\n", maxLineLength: 4,
			want: []string{"abcd\n", "abcd", "efgh", "ij\n", "z\n"}, wantWarnings: 1},
		{name: "CRLF entry of exactly the limit arrives whole", stdout: "abc\r\nd\r\n", maxLineLength: 4,
			want: []string{"abc\r\n", "d\r\n"}},
		{name: "CRLF entry above the limit", stdout: "abcd\r\n", maxLineLength: 4,
			want: []string{"abcd", "\r\n"}, wantWarnings: 1},
		{name: "every sequence of cut pieces warns", stdout: "abcdef\nabcdef\nab\nabcdef\n", maxLineLength: 4,
			want: []string{"abcd", "ef\n", "abcd", "ef\n", "ab\n", "abcd", "ef\n"}, wantWarnings: 3},
		{name: "final entry without newline is dropped", stdout: "a\nlast", maxLineLength: 4,
			want: []string{"a\n"}},
		{name: "final long entry without newline keeps its cut pieces", stdout: "a\nabcdefghij", maxLineLength: 4,
			want: []string{"a\n", "abcd", "efgh"}, wantWarnings: 1},
		{name: "empty entries are kept", stdout: "\n\nabcde\n", maxLineLength: 4,
			want: []string{"\n", "\n", "abcd", "e\n"}, wantWarnings: 1},
	}
}

// TestScanStdoutAppliesMaxLineLength scans fake journalctl stdout, delivered
// all at once and, for the small cases, one byte per read, so that the pieces
// do not depend on how journalctl's writes arrive.
func TestScanStdoutAppliesMaxLineLength(t *testing.T) {
	for _, tc := range journalSplitCases() {
		t.Run(tc.name, func(t *testing.T) {
			runScanStdoutCase(t, tc, strings.NewReader(tc.stdout))
			if len(tc.stdout) < 64*1024 {
				runScanStdoutCase(t, tc, iotest.OneByteReader(strings.NewReader(tc.stdout)))
			}
		})
	}
}

func runScanStdoutCase(t *testing.T, tc journalSplitCase, stdout io.Reader) {
	t.Helper()
	messages := make(chan string, 16)
	reader := &Reader{sourceID: "journal:test.service", serverMessages: messages,
		maxLineLength: tc.maxLineLength, logger: warnLogger{}}
	processor := &captureProcessor{}
	filter := newJournalFilter(lcontext.LContext{}, processorSink{processor: processor},
		regex.NewNoop(), reader.sourceID)
	defer filter.Close()

	if err := reader.scanStdout(context.Background(), stdout, filter, nil); err != nil {
		t.Fatalf("scan stdout: %v", err)
	}
	if !reflect.DeepEqual(processor.lines, tc.want) {
		t.Fatalf("lines: got %s, want %s", describeLines(processor.lines), describeLines(tc.want))
	}
	assertLongLineWarnings(t, messages, reader.sourceID, tc.wantWarnings)
}

// TestScanStdoutStopsWhenCanceledDuringWarning cancels the read while the
// long-line warning waits for room in the server messages: the scan ends
// with the context's error and passes no piece of the long entry on.
func TestScanStdoutStopsWhenCanceledDuringWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &Reader{sourceID: "journal-id", serverMessages: make(chan string), maxLineLength: 4}
	processor := &captureProcessor{}
	filter := newJournalFilter(lcontext.LContext{}, processorSink{processor: processor},
		regex.NewNoop(), reader.sourceID)
	defer filter.Close()

	err := reader.scanStdout(ctx, strings.NewReader("abcdef\n"), filter, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", err)
	}
	if len(processor.lines) != 0 {
		t.Fatalf("canceled scan passed lines on: %q", processor.lines)
	}
}

// TestStartCutsJournalEntryAboveOneMiB is the regression test of an entry
// longer than 1 MiB, which used to end a snapshot read with "bufio.Scanner:
// token too long", losing the rest of the snapshot.
func TestStartCutsJournalEntryAboveOneMiB(t *testing.T) {
	const longLineLength = 2*mib + 10
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"before"}, LongLineLength: longLineLength},
	})
	messages := make(chan string, 4)
	reader, err := NewReader(ReaderOptions{SourceID: "journal-id", ServerMessages: messages,
		Logger: warnLogger{}})
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{},
		processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}
	long := strings.Repeat("x", longLineLength)
	want := []string{"before\n", long[:mib], long[mib : 2*mib], long[2*mib:] + "\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("lines: got %s, want %s", describeLines(processor.lines), describeLines(want))
	}
	assertLongLineWarnings(t, messages, "journal-id", 1)
}

// TestStartFollowCutsLongEntryAndKeepsFollowing checks that a follow read
// cuts a long entry at MaxLineLength and goes on with the entries after it in
// the same journalctl run, instead of ending the run with an error (after
// which the caller restarted journalctl with -n 0, losing the entries written
// meanwhile).
func TestStartFollowCutsLongEntryAndKeepsFollowing(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:          []string{"before", strings.Repeat("y", 10)},
			FollowLines:    []string{"followed"},
			InterLineDelay: 5 * time.Millisecond,
		},
	})
	// The mock repeats its follow entry, each one cut and warned about, so
	// leave room for the warnings of the whole test.
	messages := make(chan string, 4096)
	reader, err := NewReader(ReaderOptions{SourceID: "journal-id", Follow: true,
		ServerMessages: messages, MaxLineLength: 4})
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	processor := &flushCountingProcessor{}
	done := make(chan error, 1)
	go func() { done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop()) }()

	want := []string{"befo", "re\n", "yyyy", "yyyy", "yy\n", "foll", "owed\n"}
	got := waitForLines(t, processor, len(want), done)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("follow reader returned error after cancel: %v", err)
	}
	if !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("follow lines: got %q, want %q", got, want)
	}
	if len(messages) < 2 {
		t.Fatalf("got %d long-line warnings, want at least 2 (one per long entry)", len(messages))
	}
}

func waitForLines(t *testing.T, processor *flushCountingProcessor, n int, done <-chan error) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		got := processor.snapshot()
		if len(got) >= n {
			return got
		}
		select {
		case err := <-done:
			t.Fatalf("follow reader ended early with %v after %q", err, got)
		case <-deadline:
			t.Fatalf("follow reader delivered %q, want at least %d lines", got, n)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func assertLongLineWarnings(t *testing.T, messages chan string, sourceID string, want int) {
	t.Helper()
	close(messages)
	got := 0
	for message := range messages {
		if message != fs.LongLineWarning(warnLogger{}, sourceID) ||
			!strings.Contains(message, "Long log line") {
			t.Fatalf("unexpected server message %q", message)
		}
		got++
	}
	if got != want {
		t.Fatalf("got %d long-line warnings, want %d", got, want)
	}
}

// describeLines shortens long lines for failure messages.
func describeLines(lines []string) string {
	parts := make([]string, len(lines))
	for i, line := range lines {
		if len(line) > 32 {
			parts[i] = strconv.Quote(line[:8]) + "...(" + strconv.Itoa(len(line)) + " bytes)"
			continue
		}
		parts[i] = strconv.Quote(line)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
