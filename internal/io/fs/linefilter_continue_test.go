package fs

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// TestStartFilteredContinuesALineFilter feeds the first lines of a file to a
// LineFilter, as a shared reader does, then lets a private follow reader that
// starts at the next line boundary go on through the same filter: the lines,
// their numbers and the local context must be those of one uninterrupted read.
func TestStartFilteredContinuesALineFilter(t *testing.T) {
	content := "INFO 1\nERROR 2\nINFO 3\n\nINFO 4\nERROR 5\nINFO 6\nINFO 7\nINFO 8\nERROR 9\nINFO 10\n"
	lines := strings.SplitAfter(content, "\n")
	lines = lines[:len(lines)-1]
	re := mustFlaggedRegex(t, "ERROR", regex.Default)
	contexts := []lcontext.LContext{{}, {BeforeContext: 2}, {AfterContext: 2},
		{BeforeContext: 1, AfterContext: 1}}

	for _, ltx := range contexts {
		want := &captureProcessor{}
		wantFilter := NewLineFilter(ltx, want, re, "glob")
		feedFollowLines(t, wantFilter, lines)
		wantFilter.Close()

		for split := 1; split < len(lines); split++ {
			t.Run(fmt.Sprintf("%+v/split%d", ltx, split), func(t *testing.T) {
				path := writeStartOffsetTestFile(t, "continue.log", content)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				got := &cancelOnLineProcessor{want: len(want.lines), cancel: cancel}
				filter := NewLineFilter(ltx, got, re, "glob")
				defer filter.Close()
				head := lines[:split]
				feedFollowLines(t, filter, head)

				reader := mustNewReadFile(ReadOptions{
					Mode:            omode.TailClient,
					FilePath:        path,
					GlobID:          "glob",
					StartOffset:     int64(len(strings.Join(head, ""))),
					StartOffsetFile: statStartOffsetTestFile(t, path),
					Logger:          testLogger,
				})
				if err := reader.StartFiltered(ctx, filter); err != nil {
					t.Fatalf("StartFiltered() error = %v", err)
				}
				if !reflect.DeepEqual(got.lines, want.lines) || !reflect.DeepEqual(got.lineNums, want.lineNums) {
					t.Errorf("lines %q %v, want %q %v", got.lines, got.lineNums, want.lines, want.lineNums)
				}
			})
		}
	}
}

// feedFollowLines feeds lines, which end with their newline, in the form a
// follow reader feeds them: without the newline, skipping empty lines.
func feedFollowLines(t *testing.T, filter *LineFilter, lines []string) {
	t.Helper()
	for _, text := range lines {
		if text == "\n" {
			continue
		}
		if _, err := filter.ProcessLine([]byte(strings.TrimSuffix(text, "\n"))); err != nil {
			t.Fatal(err)
		}
	}
}
