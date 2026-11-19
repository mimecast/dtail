package clients

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestEditInputTranslatesAltEAndAltV(t *testing.T) {
	key := string(externalEditKey)
	tests := []struct {
		name, in, want string
	}{
		{"alt-e", "\x1be", key},
		{"alt-v", "\x1bv", key},
		{"inside a line", ":sh\x1beow", ":sh" + key + "ow"},
		{"two", "\x1be\x1bv", key + key},
		{"other alt key untouched", "\x1bb\x1bf", "\x1bb\x1bf"},
		{"csi untouched", "\x1b[A\x1b[3~", "\x1b[A\x1b[3~"},
		{"ss3 untouched", "\x1bOe", "\x1bOe"},
		{"esc at the end kept", "abc\x1b", "abc\x1b"},
		{"capital E untouched", "\x1bE", "\x1bE"},
		{"no edit key in a bracketed paste, its ESC a blank", "\x1b[200~a\x1beb\x1b[201~\x1be",
			"\x1b[200~a eb\x1b[201~" + key},
		{"plain text", "hello", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &editInput{r: strings.NewReader(tt.in)}
			buf := make([]byte, 256)
			n, err := in.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(buf[:n]); got != tt.want {
				t.Fatalf("Read(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEditInputFitsTheBuffer checks that a read filled with Alt-E sequences
// still fits the caller's buffer after the translation (each grows from 2
// to 4 bytes), so no key is lost or cut.
func TestEditInputFitsTheBuffer(t *testing.T) {
	in := &editInput{r: strings.NewReader(strings.Repeat("\x1be", 64))}
	buf := make([]byte, 16)
	var got []byte
	for {
		n, err := in.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil || n == 0 {
			break
		}
	}
	want := bytes.Repeat([]byte(string(externalEditKey)), 64)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes %q, want 64 edit keys", len(got), got)
	}
}

func TestEditedLine(t *testing.T) {
	tests := []struct {
		in, want string
		fails    bool
	}{
		{in: ":show\n", want: ":show"},
		{in: ":query from STATS\r\n select count($line)\n\n", want: ":query from STATS select count($line)"},
		{in: "  :reload --grep 'a b'  \n", want: ":reload --grep 'a b'"},
		{in: "", want: ""},
		{in: "select\tcount($line)\x1b[1m\n", want: "select count($line) [1m"},
		{in: "\xff\n", fails: true},
		{in: strings.Repeat("x", editorMaxLine), fails: true},
	}
	for _, tt := range tests {
		got, err := editedLine(tt.in)
		if tt.fails {
			if err == nil {
				t.Errorf("editedLine(%q) = %q, want an error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("editedLine(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestExternalEditorCommand(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	tests := []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"VISUAL": "hx", "EDITOR": "vim"}, "hx"},
		{map[string]string{"VISUAL": "  ", "EDITOR": "code -w"}, "code -w"},
		{map[string]string{}, externalEditFallback},
	}
	for _, tt := range tests {
		if got := externalEditorCommand(env(tt.vars)); got != tt.want {
			t.Errorf("externalEditorCommand(%v) = %q, want %q", tt.vars, got, tt.want)
		}
	}
}

// recordingMode records the terminal mode changes of the line editor.
type recordingMode struct {
	calls []string
	// background has the mode report that dtail runs in the background.
	background bool
}

func (m *recordingMode) foreground() bool { return !m.background }

func (m *recordingMode) reapply() error { m.calls = append(m.calls, "cbreak"); return nil }
func (m *recordingMode) restore() error { m.calls = append(m.calls, "cooked"); return nil }

func newLifecycleTestEditor(out *bytes.Buffer) (*interactiveEditor, *recordingMode, *int) {
	mode := &recordingMode{}
	resized := 0
	e := &interactiveEditor{mode: mode, stop: func() {}, setForceExitHook: func(func()) {},
		screen: newEditorScreen(out), vi: newViEditor(nil, nil)}
	e.resize = func() {
		if !e.inEditor() {
			resized++
		}
	}
	return e, mode, &resized
}

// TestExternalEditorTerminalLifecycle checks the terminal around the
// external editor: cooked mode without bracketed paste for the editor,
// cbreak, bracketed paste and the window size taken over after it; when the
// session ends while the editor runs, dtail leaves mode and cursor shape to
// the editor and sets nothing afterwards, and a later Alt-E runs no editor.
func TestExternalEditorTerminalLifecycle(t *testing.T) {
	var out bytes.Buffer
	e, mode, resized := newLifecycleTestEditor(&out)
	e.runEditor = func(path string) error {
		if !e.inEditor() {
			t.Error("editing not marked while the editor runs")
		}
		e.resize() // A SIGWINCH meanwhile redraws nothing.
		return os.WriteFile(path, []byte(":show\n"), 0o600)
	}
	if line, err := e.runExternalEditor(":sh"); err != nil || line != ":show" {
		t.Fatalf("edit: %q, %v", line, err)
	}
	if got := strings.Join(mode.calls, ","); got != "cooked,cbreak" || *resized != 1 {
		t.Fatalf("modes %q, resized %d times; want cooked,cbreak and one resize after", got, *resized)
	}
	if got := out.String(); got != pasteModeOff+pasteModeOn {
		t.Fatalf("wrote %q around the editor, want bracketed paste off and on again", got)
	}

	out.Reset()
	e, mode, resized = newLifecycleTestEditor(&out)
	e.runEditor = func(path string) error {
		e.close() // The session ends while the editor runs.
		return os.WriteFile(path, []byte(":show\n"), 0o600)
	}
	if _, err := e.runExternalEditor(":sh"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mode.calls, ","); got != "cooked" || *resized != 0 || out.String() != pasteModeOff {
		t.Fatalf("closed while editing: modes %q, %d resizes, wrote %q; want only cooked and bracketed "+
			"paste off for the editor", got, *resized, out.String())
	}
	e.runEditor = func(string) error {
		t.Error("an editor ran after the session ended")
		return nil
	}
	if _, err := e.runExternalEditor(":sh"); !errors.Is(err, errEditorSessionEnded) {
		t.Fatalf("Alt-E after the end: %v, want %v", err, errEditorSessionEnded)
	}
}

// TestEditInputBracketedPaste reads pastes as the Terminal does (one chunk
// per write of the terminal, chunkReader): every
// pasted line comes with a Read of its own, control characters of a paste
// are blanks (and only there), a paste marker cut by a read is put together
// before the Terminal sees it, and onPaste is called exactly for pasted
// text that stays on the line.
func TestEditInputBracketedPaste(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		size   int
		want   []string
		pastes int
	}{
		{name: "lines of a paste one per read, none stays on the line",
			chunks: []string{"\x1b[200~:show\n:quit\n\x1b[201~"},
			want:   []string{"\x1b[200~:show\n", ":quit\n", "\x1b[201~"}},
		{name: "control characters of a paste are blanks",
			chunks: []string{"\x1b[200~ab\tc\x03\x7f\x1b[201~"},
			want:   []string{"\x1b[200~ab c  \x1b[201~"}, pastes: 1},
		{name: "end marker cut by a short read, control keys typed after it kept",
			chunks: []string{"\x1b[200~abc\x1b[20", "1~", "\x01\t"},
			want:   []string{"\x1b[200~abc", "\x1b[201~", "\x01\t"}, pastes: 1},
		{name: "start marker cut after more than its ESC",
			chunks: []string{"\x1b[2", "00~a\x1b[201~"},
			want:   []string{"\x1b[200~a\x1b[201~"}, pastes: 1},
		{name: "a lone ESC ending a short read is the Esc key",
			chunks: []string{"\x1b", "x"},
			want:   []string{"\x1b", "x"}},
		{name: "an ESC ending a read inside a paste is no end marker",
			chunks: []string{"\x1b[200~a\x1b", "b\x1b[201~"},
			want:   []string{"\x1b[200~a", " b\x1b[201~"}, pastes: 2},
		{name: "CR LF is one line break, the rest stays on the line",
			chunks: []string{"\x1b[200~a\r\nb\x1b[201~"},
			want:   []string{"\x1b[200~a\r\n", "b\x1b[201~"}, pastes: 1},
		{name: "a small buffer cuts no marker (text read along with a paste announces it too)",
			chunks: []string{"ab\x1b[200~c\x1b[201~"}, size: 7,
			want: []string{"ab", "\x1b[200~c", "\x1b[201~"}, pastes: 2},
		{name: "C1 controls and format characters of a paste are blanks, text is kept",
			chunks: []string{"\x1b[200~a\u0085b\u202ec\u2028d\u00e9\u20ac\x1b[201~\u202e"},
			want:   []string{"\x1b[200~a b c d\u00e9\u20ac\x1b[201~\u202e"}, pastes: 1},
		{name: "a format character cut by a read is still a blank",
			chunks: []string{"\x1b[200~a\xe2\x80", "\xaeb\x1b[201~"},
			want:   []string{"\x1b[200~a", " b\x1b[201~"}, pastes: 2},
		{name: "an invalid byte of a paste is passed on",
			chunks: []string{"\x1b[200~a\xffb\x1b[201~"},
			want:   []string{"\x1b[200~a\xffb\x1b[201~"}, pastes: 1},
		{name: "typed text announces no paste",
			chunks: []string{":show", "\n"},
			want:   []string{":show", "\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pastes := 0
			in := &editInput{r: &chunkReader{chunks: tt.chunks}, onPaste: func() { pastes++ }}
			size := tt.size
			if size == 0 {
				size = 256
			}
			buf := make([]byte, size)
			var got []string
			for {
				n, err := in.Read(buf)
				if n > 0 {
					got = append(got, string(buf[:n]))
				}
				if err != nil {
					break
				}
			}
			if !slices.Equal(got, tt.want) || pastes != tt.pastes {
				t.Fatalf("reads %q with %d pastes announced, want %q with %d", got, pastes, tt.want, tt.pastes)
			}
		})
	}
}

// TestPasteModeLeftAloneInBackground: like the terminal mode, bracketed
// paste is not changed while dtail runs in the background, where the
// terminal is the shell's (the SIGCONT of "bg" must not switch it on).
func TestPasteModeLeftAloneInBackground(t *testing.T) {
	var out bytes.Buffer
	e, mode, _ := newLifecycleTestEditor(&out)
	mode.background = true
	e.setPasteMode(true)
	e.setPasteMode(false)
	if out.Len() != 0 {
		t.Fatalf("in the background the editor wrote %q, want nothing", out.String())
	}
	mode.background = false
	e.setPasteMode(true)
	e.setPasteMode(false)
	if got := out.String(); got != pasteModeOn+pasteModeOff {
		t.Fatalf("in the foreground the editor wrote %q, want bracketed paste on and off", got)
	}
}
