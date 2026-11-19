package clients

import (
	"bytes"
	"errors"
	"os"
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
		{"bracketed paste untouched", "\x1b[200~a\x1beb\x1b[201~\x1be",
			"\x1b[200~a\x1beb\x1b[201~" + key},
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
type recordingMode struct{ calls []string }

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
// external editor: cooked mode for the editor, cbreak and the window size
// taken over after it; when the session ends while the editor runs, dtail
// leaves mode and cursor shape to the editor and sets nothing afterwards,
// and a later Alt-E runs no editor.
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

	out.Reset()
	e, mode, resized = newLifecycleTestEditor(&out)
	e.runEditor = func(path string) error {
		e.close() // The session ends while the editor runs.
		return os.WriteFile(path, []byte(":show\n"), 0o600)
	}
	if _, err := e.runExternalEditor(":sh"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mode.calls, ","); got != "cooked" || *resized != 0 || out.Len() != 0 {
		t.Fatalf("closed while editing: modes %q, %d resizes, wrote %q; want only cooked for the editor",
			got, *resized, out.String())
	}
	e.runEditor = func(string) error {
		t.Error("an editor ran after the session ended")
		return nil
	}
	if _, err := e.runExternalEditor(":sh"); !errors.Is(err, errEditorSessionEnded) {
		t.Fatalf("Alt-E after the end: %v, want %v", err, errEditorSessionEnded)
	}
}
