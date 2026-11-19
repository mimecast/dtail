package fs

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// handOverCall is one question a follow reader asked HandOverAtEOF.
type handOverCall struct {
	offset int64
	same   bool
}

func appendToTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := appendFile(path, text); err != nil {
		t.Fatal(err)
	}
}

func appendFile(path, text string) error {
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fd.WriteString(text); err != nil {
		_ = fd.Close()
		return err
	}
	return fd.Close()
}

// TestFollowReadHandsOverAtTheEndOfTheFile checks that a follow read asks
// HandOverAtEOF only at the end of the file at the start of a line, with the
// end of the file and its identity, keeps reading when it declines, and ends
// with ErrHandedOver, having fed exactly the lines before that offset, when it
// accepts. The callback runs on the reader's goroutine, so it appends to the
// file deterministically.
func TestFollowReadHandsOverAtTheEndOfTheFile(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		start         int64
		maxLineLength int
		// appends[i] is appended at the i-th question, which is declined;
		// the question after the last append is accepted.
		appends []string
		// lateAppend is appended by another goroutine after a while, with
		// the reader polling the end of the file meanwhile.
		lateAppend string
		wantLines  []string
		wantAsked  []int64
	}{
		{name: "at once", content: "a\nb\n", wantLines: []string{"a", "b"}, wantAsked: []int64{4}},
		{name: "empty file", content: "", wantAsked: []int64{0}},
		{name: "declined then more lines", content: "a\n", appends: []string{"b\nc\n"},
			wantLines: []string{"a", "b", "c"}, wantAsked: []int64{2, 6}},
		{name: "unfinished line is not handed over", content: "a\nb", lateAppend: "c\nd\n",
			// Never asked while "b" is pending, only once its line ended.
			wantLines: []string{"a", "bc", "d"}, wantAsked: []int64{7}},
		{name: "starts in the middle of a line", content: "abc\nd\n", start: 1,
			wantLines: []string{"bc", "d"}, wantAsked: []int64{6}},
		{name: "starts at a line boundary", content: "abc\nd\n", start: 4,
			wantLines: []string{"d"}, wantAsked: []int64{6}},
		{name: "starts at the end after a newline", content: "abc\n", start: 4, wantAsked: []int64{4}},
		{name: "starts at the end in a line", content: "abc", start: 2, lateAppend: "\n",
			wantLines: []string{"c"}, wantAsked: []int64{4}},
		{name: "starts at the end of an unfinished line", content: "abc", start: 3, lateAppend: "\nd\n",
			wantLines: []string{"d"}, wantAsked: []int64{6}},
		{name: "split long line", content: "abcdefgh", maxLineLength: 4, lateAppend: "ij\n",
			wantLines: []string{"abcdefgh", "ij"}, wantAsked: []int64{11}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeStartOffsetTestFile(t, "handover.log", tt.content)
			identity := statStartOffsetTestFile(t, path)
			appends := tt.appends
			if tt.lateAppend != "" {
				timer := time.AfterFunc(300*time.Millisecond, func() {
					if err := appendFile(path, tt.lateAppend); err != nil {
						t.Error(err)
					}
				})
				defer timer.Stop()
			}
			var asked []handOverCall
			options := ReadOptions{
				Mode: omode.TailClient, FilePath: path, GlobID: "glob", Logger: testLogger,
				MaxLineLength: tt.maxLineLength,
				HandOverAtEOF: func(offset int64, file os.FileInfo) bool {
					asked = append(asked, handOverCall{offset: offset, same: os.SameFile(file, identity)})
					if len(appends) > 0 {
						appendToTestFile(t, path, appends[0])
						appends = appends[1:]
						return false
					}
					return true
				},
			}
			if tt.start > 0 {
				options.StartOffset, options.StartOffsetFile = tt.start, identity
			}
			reader := mustNewReadFile(options)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got := &captureProcessor{}
			filter := NewLineFilter(lcontext.LContext{}, got, regex.NewNoop(), "glob")
			defer filter.Close()

			if err := reader.StartFiltered(ctx, filter); !errors.Is(err, ErrHandedOver) {
				t.Fatalf("StartFiltered() = %v, want ErrHandedOver", err)
			}
			if !reflect.DeepEqual(got.lines, tt.wantLines) {
				t.Errorf("lines = %q, want %q", got.lines, tt.wantLines)
			}
			var offsets []int64
			for _, call := range asked {
				offsets = append(offsets, call.offset)
				if !call.same {
					t.Errorf("asked with another file's identity at %d", call.offset)
				}
			}
			if !reflect.DeepEqual(offsets, tt.wantAsked) {
				t.Errorf("asked at %v, want %v", offsets, tt.wantAsked)
			}
		})
	}
}

// TestHandedOverReadKeepsLocalContext checks that the before-context a follow
// read buffered survives the hand-over, so that the filter fed from elsewhere
// afterwards sends it with the next match, like one uninterrupted read.
func TestHandedOverReadKeepsLocalContext(t *testing.T) {
	path := writeStartOffsetTestFile(t, "context.log", "INFO 1\nINFO 2\nERROR 3\nINFO 4\nINFO 5\n")
	re := mustFlaggedRegex(t, "ERROR", regex.Default)
	ltx := lcontext.LContext{BeforeContext: 2, AfterContext: 1}
	got := &captureProcessor{}
	filter := NewLineFilter(ltx, got, re, "glob")
	defer filter.Close()
	reader := mustNewReadFile(ReadOptions{
		Mode: omode.TailClient, FilePath: path, GlobID: "glob", Logger: testLogger,
		HandOverAtEOF: func(int64, os.FileInfo) bool { return true },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reader.StartFiltered(ctx, filter); !errors.Is(err, ErrHandedOver) {
		t.Fatalf("StartFiltered() = %v, want ErrHandedOver", err)
	}
	for _, text := range []string{"INFO 6", "ERROR 7", "INFO 8"} {
		if _, err := filter.ProcessLine([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}

	wantLines := []string{"INFO 1", "INFO 2", "ERROR 3", "INFO 4", "INFO 5", "INFO 6", "ERROR 7", "INFO 8"}
	wantNums := []uint64{1, 2, 3, 4, 5, 6, 7, 8}
	if !reflect.DeepEqual(got.lines, wantLines) || !reflect.DeepEqual(got.lineNums, wantNums) {
		t.Errorf("lines %q %v, want %q %v", got.lines, got.lineNums, wantLines, wantNums)
	}
}

// TestHandOverAfterATruncation checks that a follow read that rewound a
// truncated file hands over at its new end.
func TestHandOverAfterATruncation(t *testing.T) {
	path := writeStartOffsetTestFile(t, "truncated.log", "old 1\nold 2\n")
	identity := statStartOffsetTestFile(t, path)
	var asked []int64
	reader := mustNewReadFile(ReadOptions{
		Mode: omode.TailClient, FilePath: path, GlobID: "glob", Logger: testLogger,
		StartOffset: 12, StartOffsetFile: identity,
		HandOverAtEOF: func(offset int64, _ os.FileInfo) bool {
			asked = append(asked, offset)
			if len(asked) == 1 {
				if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return false
			}
			return true
		},
	})
	got := &captureProcessor{}
	filter := NewLineFilter(lcontext.LContext{}, got, regex.NewNoop(), "glob")
	defer filter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reader.StartFiltered(ctx, filter); !errors.Is(err, ErrHandedOver) {
		t.Fatalf("StartFiltered() = %v, want ErrHandedOver", err)
	}
	if want := []string{"new"}; !reflect.DeepEqual(got.lines, want) {
		t.Errorf("lines = %q, want %q", got.lines, want)
	}
	if want := []int64{12, 4}; !reflect.DeepEqual(asked, want) {
		t.Errorf("asked at %v, want %v", asked, want)
	}
}

func TestNewReadFileValidatesHandOver(t *testing.T) {
	handOver := func(int64, os.FileInfo) bool { return true }
	tests := []struct {
		name    string
		options ReadOptions
		wantErr string
	}{
		{"plain file", ReadOptions{FilePath: "/tmp/a.log"}, ""},
		{"stdin pipe", ReadOptions{GlobID: "-"}, "stdin pipe"},
		{"gz file", ReadOptions{FilePath: "/tmp/a.log.gz"}, "compressed"},
		{"zst file", ReadOptions{FilePath: "/tmp/a.log.zst"}, "compressed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := tt.options
			options.Mode = omode.TailClient
			options.HandOverAtEOF = handOver
			_, err := NewReadFile(options)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("NewReadFile() error = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("NewReadFile() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
