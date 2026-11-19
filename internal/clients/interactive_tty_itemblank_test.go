package clients

import (
	"io"
	"strings"
	"testing"

	"golang.org/x/term"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// newItemBlankTestEditor returns a client of mode args and a line editor
// whose keys go to its editorKey, for the item blank tests.
func newItemBlankTestEditor(args config.Args, vi bool) (*baseClient, *interactiveEditor, *interactivePause) {
	client := newPauseTestClient(newPausingTestLogger())
	client.Args = args
	pause := client.newInteractivePause()
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), io.Discard}, "")
	editor := &interactiveEditor{terminal: terminal}
	if vi {
		editor.vi = newViEditor(viTestHistory{}, nil)
	}
	return client, editor, pause
}

// tabAfterItem presses Tab at the end of line and checks that it inserted
// the blank after the finished item that ends it.
func tabAfterItem(t *testing.T, client *baseClient, editor *interactiveEditor, pause *interactivePause,
	line string) (string, int) {

	t.Helper()
	got, pos, ok := client.editorKey(editor, pause, line, len(line), '\t')
	if !ok || got != line+" " || pos != len(got) {
		t.Fatalf("Tab after %q = %q at %d (%v), want the blank after the item", line, got, pos, ok)
	}
	return got, pos
}

// TestEditorCommaReplacesItemBlank checks that a comma typed right after
// the blank Tab inserted after a finished query item replaces that blank,
// for :query and a quoted :reload --query value, and in vi insert mode.
func TestEditorCommaReplacesItemBlank(t *testing.T) {
	tail := config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	dmap := config.Args{Mode: omode.MapClient, What: "/var/log/app.log", QueryStr: switchTestQuery}
	for _, tt := range []struct {
		name, line string
		args       config.Args
		vi         bool
	}{
		{name: ":query", args: tail, line: ":query from STATS select count($line)"},
		{name: ":query vi insert mode", args: tail, line: ":query from STATS select count($line)", vi: true},
		{name: "--query value", args: dmap, line: ":reload --query 'from STATS select count($line)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, editor, pause := newItemBlankTestEditor(tt.args, tt.vi)
			line, pos := tabAfterItem(t, client, editor, pause, tt.line)
			got, gotPos, ok := client.editorKey(editor, pause, line, pos, ',')
			want := tt.line + ","
			if !ok || got != want || gotPos != len(want) {
				t.Fatalf("comma after the item blank = %q at %d (%v), want %q", got, gotPos, ok, want)
			}
			// Only once: a second comma is typed as it is.
			if _, _, ok := client.editorKey(editor, pause, got, gotPos, ','); ok {
				t.Fatal("a second comma was taken by the editor hook")
			}
		})
	}
}

// TestEditorCommaKeepsOtherBlanks checks that a comma is typed as it is
// after a blank Tab did not insert after an item: one typed by hand, one
// after which another key was typed, with the cursor moved away or the line
// changed, and in vi normal mode, where the comma is a motion.
func TestEditorCommaKeepsOtherBlanks(t *testing.T) {
	tail := config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	const item = ":query from STATS select count($line)"
	client, editor, pause := newItemBlankTestEditor(tail, false)
	if _, _, ok := client.editorKey(editor, pause, item+" ", len(item)+1, ','); ok {
		t.Fatal("comma after a typed blank was taken by the editor hook")
	}

	line, pos := tabAfterItem(t, client, editor, pause, item)
	// Another key (here a blank typed and erased again with Backspace,
	// which the hook does not see) ends it.
	if _, _, ok := client.editorKey(editor, pause, line, pos, ' '); ok {
		t.Fatal("blank was taken by the editor hook")
	}
	if _, _, ok := client.editorKey(editor, pause, line, pos, ','); ok {
		t.Fatal("comma after another key replaced the blank")
	}

	line, pos = tabAfterItem(t, client, editor, pause, item)
	if _, _, ok := client.editorKey(editor, pause, line, pos-1, ','); ok {
		t.Fatal("comma with the cursor moved before the blank replaced it")
	}
	line, pos = tabAfterItem(t, client, editor, pause, item)
	if _, _, ok := client.editorKey(editor, pause, "x"+line, pos+1, ','); ok {
		t.Fatal("comma on a changed line replaced the blank")
	}

	client, editor, pause = newItemBlankTestEditor(tail, true)
	line, pos = tabAfterItem(t, client, editor, pause, item)
	editor.vi.mode = viNormal
	if got, _, ok := client.editorKey(editor, pause, line, pos-1, ','); !ok || got != line {
		t.Fatalf("comma in vi normal mode = %q (%v), want the vi motion on %q", got, ok, line)
	}
}
