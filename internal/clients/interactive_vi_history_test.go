package clients

import (
	"io"
	"strings"
	"testing"

	"golang.org/x/term"
)

// chunkReader hands out one chunk per Read, as a terminal sends one key's
// bytes per write, and then io.EOF.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	if n == len(r.chunks[0]) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = r.chunks[0][n:]
	}
	return n, nil
}

// readViLine types keys, one chunk per key, into a real x/term Terminal
// wired like the line editor in vi mode (editInput, controlHistory,
// editorKey) with the history entries, most recent first, and returns the
// line Enter gave. In keys, ESC stands for the Esc key, U and D for the Up
// and Down arrows, P and N for Ctrl-P and Ctrl-N.
func readViLine(t *testing.T, entries []string, keys string) string {
	t.Helper()
	line, _ := readViLinePause(t, entries, keys)
	return line
}

// readViLinePause is readViLine, also returning the output pause the keys
// left behind.
func readViLinePause(t *testing.T, entries []string, keys string) (string, *interactivePause) {
	t.Helper()
	var chunks []string
	for _, key := range keys {
		chunks = append(chunks, map[rune]string{'U': "\x1b[A", 'D': "\x1b[B", 'P': "\x10", 'N': "\x0e"}[key])
		if chunks[len(chunks)-1] == "" {
			chunks[len(chunks)-1] = string(key)
		}
	}
	chunks = append(chunks, "\r")
	client := newPauseTestClient(newPausingTestLogger())
	pause := client.newInteractivePause()
	input := editorTTY{Reader: &editInput{r: &chunkReader{chunks: chunks}, viEscape: true}, Writer: io.Discard}
	editor := &interactiveEditor{terminal: term.NewTerminal(input, "")}
	editor.bindKeys(client, pause, true, io.Discard)
	for i := len(entries) - 1; i >= 0; i-- {
		editor.terminal.History.Add(entries[i])
	}
	line, err := editor.ReadLine()
	if err != nil {
		t.Fatalf("keys %q: ReadLine: %v", keys, err)
	}
	return line, pause
}

// TestViArrowsPauseOnlyOnRecall checks that Up and Down pause the output
// for typing when they recall a line, as in the emacs mode, and not when
// there is nothing to recall: such a key changes nothing on the line and
// must not freeze the output unnoticed.
func TestViArrowsPauseOnlyOnRecall(t *testing.T) {
	if line, pause := readViLinePause(t, nil, "UD"); line != "" || pause.paused {
		t.Fatalf("arrows with an empty history: line %q, paused %v, want no line and no pause", line, pause.paused)
	}
	if line, pause := readViLinePause(t, []string{":show"}, "D"); line != "" || pause.paused {
		t.Fatalf("Down without a recall: line %q, paused %v, want no line and no pause", line, pause.paused)
	}
	if line, pause := readViLinePause(t, []string{":show"}, "U"); line != ":show" || !pause.paused || !pause.typing {
		t.Fatalf("Up: line %q, paused %v, typing %v, want the entry and a typing pause", line, pause.paused, pause.typing)
	}
}

// TestViHistoryOnePosition checks through a real Terminal that the arrows
// and k/j move one history position: each goes on from where the other
// left, and Down or j past the newest entry gives back the line that was
// being edited, whichever key left it.
func TestViHistoryOnePosition(t *testing.T) {
	entries := []string{":show", ":help", ":probe"}
	const esc = "\x1b"
	tests := []struct {
		name, keys, want string
	}{
		{"k then Up goes on to the older entry", "dra" + esc + "kU", ":help"},
		{"Up then k goes on to the older entry", "draU" + esc + "k", ":help"},
		{"k Up Down is the entry of k", "dra" + esc + "kUD", ":show"},
		{"k Up Down Down restores the draft", "dra" + esc + "kUDD", "dra"},
		{"Up Up j j restores the draft", "draUU" + esc + "jj", "dra"},
		{"k k Down j restores the draft", "dra" + esc + "kkDj", "dra"},
		{"Up j restores the draft", "draU" + esc + "j", "dra"},
		{"Up Down in insert mode restores the draft", "draUD", "dra"},
		{"Ctrl-P then k goes on to the older entry", "draP" + esc + "k", ":help"},
		{"k then Ctrl-P goes on to the older entry", "dra" + esc + "kP", ":help"},
		{"Ctrl-P Up Down Down restores the draft", "draPUDD", "dra"},
		{"k k Ctrl-N j restores the draft", "dra" + esc + "kkNj", "dra"},
		{"Up in insert mode edits the entry at its end", "draUx", ":showx"},
		{"Up in normal mode ends on the last character", "dra" + esc + "Ux", ":sho"},
		{"Up past the oldest entry stays", "draUUUUU", ":probe"},
		{"Down without a recall keeps the line", "draD", "dra"},
		{"a draft equal to an entry is no recall", ":show" + esc + "k", ":show"},
		{"an arrow ends a count", "dra" + esc + "3Uk", ":help"},
		{"an arrow ends an operator", "dra" + esc + "dUw", ":show"},
		{"an arrow is no character for f", "dra" + esc + "fUx", ":sho"},
		{"undo belongs to the recalled line", "dra" + esc + "kxUu", ":help"},
		{"insert after a recall is one undo step of that line", "draUx" + esc + "u", ":show"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readViLine(t, entries, tt.keys); got != tt.want {
				t.Fatalf("keys %q gave %q, want %q", strings.ReplaceAll(tt.keys, esc, "<Esc>"), got, tt.want)
			}
		})
	}
	// An empty history: the arrows change nothing and are not typed.
	if got := readViLine(t, nil, "draUD"+esc+"kjUD"); got != "dra" {
		t.Fatalf("arrows with an empty history gave %q, want %q", got, "dra")
	}
}
