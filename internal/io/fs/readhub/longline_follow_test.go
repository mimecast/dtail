package readhub

import (
	"reflect"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

// TestSharedFollowCutsLongLinesLikePrivateFollow checks that a session of a
// shared follow read gets lines of exactly MaxLineLength whole and longer ones
// cut at exactly the limit, the same pieces with the same numbering and long
// line warnings as a private follow read, also when a writer writes a line in
// several parts.
func TestSharedFollowCutsLongLinesLikePrivateFollow(t *testing.T) {
	const maxLineLength = 8
	hub := New(Options{Logger: logging.NopLogger{}, RetryInterval: 10 * time.Millisecond,
		MaxLineLength: maxLineLength})
	file := newTestFile(t)
	shared := startFollower(t, hub, file, lcontext.LContext{}, regex.NewNoop(), "glob")
	privateMessages := make(chan string, 16)
	private := startPrivateFollower(t, file, lcontext.LContext{}, regex.NewNoop(), maxLineLength, privateMessages)
	marker := syncWithPrivate(t, file, shared, private)

	// Each entry is written at once; the reads in between see the parts.
	writes := [][]string{
		{"xxxxxxxx\n"},
		{"yyyyyyyyz\n"},
		{"aaaa", "bbbbcc", "c\n"},
		{"dddddddd", "\n"},
		{"eeeeeeeeffffffff\n"},
		{"gggggggg", "hh\n"},
		{"sync end\n"},
	}
	for _, parts := range writes {
		for _, part := range parts {
			file.appendRaw(part)
			time.Sleep(30 * time.Millisecond)
		}
	}
	waitFor(t, "both readers to finish", func() bool {
		return shared.recorder.hasLine("sync end") && private.hasLine("sync end")
	})

	want := []string{"xxxxxxxx", "yyyyyyyy", "z", "aaaabbbb", "ccc", "dddddddd",
		"eeeeeeee", "ffffffff", "gggggggg", "hh", "sync end"}
	for name, r := range map[string]*recorder{"shared": shared.recorder, "private": private} {
		lines, steps := afterMarker(r, marker)
		if !reflect.DeepEqual(lines, want) {
			t.Errorf("%s lines = %q, want %q", name, lines, want)
		}
		for i, step := range steps {
			if step != uint64(i+1) {
				t.Errorf("%s line %q is %d lines after the marker, want %d", name, lines[i], step, i+1)
			}
		}
	}
	// One warning per line that was cut.
	const wantWarnings = 4
	waitFor(t, "the long line warnings", func() bool {
		return len(shared.messages) == wantWarnings && len(privateMessages) == wantWarnings
	})
}
