package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// rotationProcessor records the lines, line numbers and line ends a follow
// reader feeds, and runs atEOF once, on the reader's goroutine, the first
// time a read returned nothing at the end of the file: after the reader's
// last read and before it checks whether the path was rotated.
type rotationProcessor struct {
	mu       sync.Mutex
	lines    []string
	numbers  []uint64
	ends     []int64
	files    []os.FileInfo
	calls    []string
	lastUpTo int64
	atEOF    func()
}

func (p *rotationProcessor) ProcessLine(buf *bytes.Buffer, lineNum uint64, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, buf.String())
	p.numbers = append(p.numbers, lineNum)
	pool.RecycleBytesBuffer(buf)
	return nil
}

func (p *rotationProcessor) Flush() error { return nil }
func (p *rotationProcessor) Close() error { return nil }

func (p *rotationProcessor) LineEndsAt(offset int64, file os.FileInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ends = append(p.ends, offset)
	p.files = append(p.files, file)
}

func (p *rotationProcessor) ReadStarting() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "starting")
}

func (p *rotationProcessor) ReadUpTo(offset int64, _ os.FileInfo) {
	p.mu.Lock()
	p.calls = append(p.calls, "up to")
	emptyRead := len(p.calls) > 1 && offset == p.lastUpTo
	p.lastUpTo = offset
	atEOF := p.atEOF
	if emptyRead {
		p.atEOF = nil
	}
	p.mu.Unlock()
	if emptyRead && atEOF != nil {
		atEOF()
	}
}

// TestFollowReadDrainsTheRotatedFile checks that a follow read that finds its
// file rotated away from the path first feeds what was appended to the old
// file after its last read, and what a writer that has not reopened the path
// yet appends to it while polls keep finding more, in order, numbered on and
// with their positions in the old file, before it ends so that the next read
// opens the new file. A trailing unfinished line is not fed.
func TestFollowReadDrainsTheRotatedFile(t *testing.T) {
	tests := []struct {
		name string
		// appendBefore is appended to the file right before the rotation,
		// after the reader's last read.
		appendBefore string
		// removeOnly leaves the path missing after the rename.
		removeOnly bool
		// polls[i] is what the old file's writer appends at the i-th poll.
		polls     []string
		wantLines []string
		wantPolls int
		wantErr   error
	}{
		{name: "nothing new", wantLines: []string{"a", "b"}, wantPolls: 1, wantErr: errFileRotated},
		{name: "appended before the rotation", appendBefore: "c\nd\n",
			wantLines: []string{"a", "b", "c", "d"}, wantPolls: 1, wantErr: errFileRotated},
		{name: "path missing after the rename", appendBefore: "c\n", removeOnly: true,
			wantLines: []string{"a", "b", "c"}, wantPolls: 1, wantErr: os.ErrNotExist},
		{name: "unfinished last line is dropped", appendBefore: "c\npart",
			wantLines: []string{"a", "b", "c"}, wantPolls: 1, wantErr: errFileRotated},
		{name: "unfinished line finished while draining", appendBefore: "c\npa", polls: []string{"rt\n"},
			wantLines: []string{"a", "b", "c", "part"}, wantPolls: 2, wantErr: errFileRotated},
		{name: "writer goes on after the rename", polls: []string{"c\n", "d\ne\n"},
			wantLines: []string{"a", "b", "c", "d", "e"}, wantPolls: 3, wantErr: errFileRotated},
		{name: "an empty poll ends draining", appendBefore: "c\n", polls: []string{"", "late\n"},
			wantLines: []string{"a", "b", "c"}, wantPolls: 1, wantErr: errFileRotated},
		{name: "draining is bounded", polls: repeatedPolls(maxRotationDrainPolls+5, "x\n"),
			wantLines: append([]string{"a", "b"}, repeatedPolls(maxRotationDrainPolls, "x")...),
			wantPolls: maxRotationDrainPolls, wantErr: errFileRotated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rotate.log")
			rotated := path + ".1"
			writeTestPath(t, path, "a\nb\n")
			writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Close() }()

			processor := &rotationProcessor{atEOF: func() {
				writeTo(t, writer, tt.appendBefore)
				if renameErr := os.Rename(path, rotated); renameErr != nil {
					t.Error(renameErr)
				}
				if !tt.removeOnly {
					writeTestPath(t, path, "new\n")
				}
			}}
			reader := mustNewReadFile(ReadOptions{Mode: omode.TailClient, FilePath: path, GlobID: "glob"})
			polls := 0
			reader.rotationDrainPoll = func(context.Context) bool {
				if polls < len(tt.polls) {
					writeTo(t, writer, tt.polls[polls])
				}
				polls++
				return true
			}

			err = startWithTimeout(t, reader, processor)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("read ended with %v, want %v", err, tt.wantErr)
			}
			if polls != tt.wantPolls {
				t.Errorf("polled %d times, want %d", polls, tt.wantPolls)
			}
			assertDrainedLines(t, processor, rotated, tt.wantLines)
		})
	}
}

// TestFollowReadDrainingRotatedFileEndsOnCancel checks that a read canceled
// while it waits to poll the rotated-away file ends without an error.
func TestFollowReadDrainingRotatedFileEndsOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotate.log")
	writeTestPath(t, path, "a\n")
	processor := &rotationProcessor{atEOF: func() {
		appendTestPath(t, path, "b\n")
		if err := os.Rename(path, path+".1"); err != nil {
			t.Error(err)
		}
	}}
	reader := mustNewReadFile(ReadOptions{Mode: omode.TailClient, FilePath: path, GlobID: "glob"})
	reader.rotationDrainPoll = func(context.Context) bool { return false }

	if err := startWithTimeout(t, reader, processor); err != nil {
		t.Fatalf("canceled read ended with %v", err)
	}
	processor.mu.Lock()
	defer processor.mu.Unlock()
	if want := []string{"a", "b"}; !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("lines = %q, want %q", processor.lines, want)
	}
}

// TestFollowReadWaitsBeforePollingTheRotatedFile checks the real wait before
// the poll of a rotated-away file: the read ends one poll interval after it
// found the rotation, when that poll finds nothing new.
func TestFollowReadWaitsBeforePollingTheRotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotate.log")
	writeTestPath(t, path, "a\n")
	var rotatedAt time.Time
	processor := &rotationProcessor{atEOF: func() {
		appendTestPath(t, path, "b\n")
		if err := os.Rename(path, path+".1"); err != nil {
			t.Error(err)
		}
		rotatedAt = time.Now()
	}}
	reader := mustNewReadFile(ReadOptions{Mode: omode.TailClient, FilePath: path, GlobID: "glob"})

	err := startWithTimeout(t, reader, processor)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read ended with %v, want %v", err, os.ErrNotExist)
	}
	if elapsed := time.Since(rotatedAt); elapsed < rotationDrainPollInterval {
		t.Errorf("read ended %v after the rotation, want at least one poll interval", elapsed)
	}
	assertDrainedLines(t, processor, path+".1", []string{"a", "b"})
}

func repeatedPolls(count int, text string) []string {
	polls := make([]string, count)
	for i := range polls {
		polls[i] = text
	}
	return polls
}

func writeTo(t *testing.T, writer *os.File, text string) {
	t.Helper()
	if text == "" {
		return
	}
	if _, err := writer.WriteString(text); err != nil {
		t.Error(err)
	}
}

func startWithTimeout(t *testing.T, reader *ReadFile, processor *rotationProcessor) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(context.Background(), lcontext.LContext{}, processor, regex.NewNoop())
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("follow read did not end after the rotation")
		return nil
	}
}

// assertDrainedLines checks that the reader fed want, numbered from 1, each
// with its end in the rotated-away file, and that ReadStarting and ReadUpTo
// alternated.
func assertDrainedLines(t *testing.T, processor *rotationProcessor, rotated string, want []string) {
	t.Helper()
	processor.mu.Lock()
	defer processor.mu.Unlock()
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("lines = %q, want %q", processor.lines, want)
	}
	oldFile, err := os.Stat(rotated)
	if err != nil {
		t.Fatal(err)
	}
	var end int64
	for i, line := range want {
		end += int64(len(line)) + 1
		if processor.numbers[i] != uint64(i+1) {
			t.Errorf("line %q numbered %d, want %d", line, processor.numbers[i], i+1)
		}
		if processor.ends[i] != end || !os.SameFile(processor.files[i], oldFile) {
			t.Errorf("line %q ends at %d in %v, want %d in the rotated file",
				line, processor.ends[i], processor.files[i].Name(), end)
		}
	}
	// The first ReadUpTo is the start position, then every read is
	// announced and followed by one.
	for i, call := range processor.calls {
		want := "starting"
		if i%2 == 0 {
			want = "up to"
		}
		if call != want {
			t.Fatalf("call %d = %q, want %q (calls %v)", i, call, want, processor.calls)
		}
	}
}
