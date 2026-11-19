package clients

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// viTestHistory is a control line history, most recent first.
type viTestHistory []string

func (h viTestHistory) Len() int          { return len(h) }
func (h viTestHistory) At(idx int) string { return h[idx] }

// runVi types keys into a vi editor starting in mode on line with the
// cursor at the rune position pos, the way the Terminal does: a key the vi
// layer does not handle is inserted when printable (insert mode). In keys,
// ESC stands for the Esc key (viEscapeKey).
func runVi(t *testing.T, v *viEditor, mode viMode, line string, pos int, keys string) (string, int) {
	t.Helper()
	v.mode = mode
	bytePos := len(string([]rune(line)[:pos]))
	for _, key := range keys {
		if key == 0x1b {
			key = viEscapeKey
		}
		newLine, newPos, ok := v.key(line, bytePos, key)
		if ok {
			line, bytePos = newLine, newPos
			continue
		}
		if insertsText(key) {
			line = line[:bytePos] + string(key) + line[bytePos:]
			bytePos += utf8.RuneLen(key)
		}
	}
	return line, utf8.RuneCountInString(line[:bytePos])
}

func TestViNormalMode(t *testing.T) {
	tests := []struct {
		line     string
		pos      int
		keys     string
		wantLine string
		wantPos  int
		wantMode viMode
	}{
		// Motions.
		{"hello world", 0, "w", "hello world", 6, viNormal},
		{"hello world", 6, "b", "hello world", 0, viNormal},
		{"hello world", 0, "e", "hello world", 4, viNormal},
		{"hello world", 0, "ee", "hello world", 10, viNormal},
		{"foo.bar baz", 0, "w", "foo.bar baz", 3, viNormal},
		{"foo.bar baz", 0, "W", "foo.bar baz", 8, viNormal},
		{"foo.bar baz", 10, "B", "foo.bar baz", 8, viNormal},
		{"foo.bar baz", 0, "E", "foo.bar baz", 6, viNormal},
		{"a b c d", 0, "3w", "a b c d", 6, viNormal},
		{"hello world", 5, "0", "hello world", 0, viNormal},
		{"hello world", 0, "$", "hello world", 10, viNormal},
		{"  hi", 3, "^", "  hi", 2, viNormal},
		{"hello world", 0, "l", "hello world", 1, viNormal},
		{"hello world", 0, "20l", "hello world", 10, viNormal},
		{"hello world", 3, "h", "hello world", 2, viNormal},
		{"hello world", 0, "fo", "hello world", 4, viNormal},
		{"hello world", 0, "fo;", "hello world", 7, viNormal},
		{"hello world", 0, "fo;,", "hello world", 4, viNormal},
		{"hello world", 0, "2fo", "hello world", 7, viNormal},
		{"hello world", 0, "to", "hello world", 3, viNormal},
		{"hello world", 10, "Fo", "hello world", 7, viNormal},
		{"hello world", 10, "To", "hello world", 8, viNormal},
		{"hello world", 0, "fz", "hello world", 0, viNormal},
		// D, the request that came with the vi mode: delete to the end.
		{"hello world", 3, "D", "hel", 2, viNormal},
		{"hello world", 0, "D", "", 0, viNormal},
		{"hello world", 3, "d$", "hel", 2, viNormal},
		// Deletes.
		{"hello world", 0, "x", "ello world", 0, viNormal},
		{"hello world", 0, "3x", "lo world", 0, viNormal},
		{"hello world", 10, "X", "hello word", 9, viNormal},
		{"hello world", 0, "dw", "world", 0, viNormal},
		{"hello world", 6, "dw", "hello ", 5, viNormal},
		{"a b c d", 0, "d2w", "c d", 0, viNormal},
		{"a b c d", 0, "2d2w", "", 0, viNormal},
		{"hello world", 6, "db", "world", 0, viNormal},
		{"hello world", 0, "de", " world", 0, viNormal},
		{"hello world", 0, "dfo", " world", 0, viNormal},
		{"hello world", 0, "dto", "o world", 0, viNormal},
		{"hello world", 10, "dFo", "hello wd", 7, viNormal},
		{"hello world", 5, "d0", " world", 0, viNormal},
		{"hello world", 4, "dd", "", 0, viNormal},
		{"hello world", 0, "d\x1bx", "ello world", 0, viNormal},
		{"hello world", 0, "dz", "hello world", 0, viNormal},
		// Changes.
		{"hello world", 0, "cwbye\x1b", "bye world", 2, viNormal},
		{"hello world", 4, "cwO\x1b", "hellO world", 4, viNormal},
		{"hello world", 0, "c2wX\x1b", "X", 0, viNormal},
		{"hello world", 6, "Cthere\x1b", "hello there", 10, viNormal},
		{"hello world", 3, "ccnew\x1b", "new", 2, viNormal},
		{"hello world", 3, "Snew\x1b", "new", 2, viNormal},
		{"hello world", 0, "sj\x1b", "jello world", 0, viNormal},
		{"hello world", 0, "cw", " world", 0, viInsert},
		// Replace, case.
		{"hello", 0, "rj", "jello", 0, viNormal},
		{"hello", 0, "3rx", "xxxlo", 2, viNormal},
		{"hello", 3, "3rx", "hello", 3, viNormal},
		{"hello", 0, "~~", "HEllo", 2, viNormal},
		{"hello", 4, "~", "hellO", 4, viNormal},
		// Yank and put.
		{"hello world", 0, "ywP", "hello hello world", 5, viNormal},
		{"hello world", 0, "yw$p", "hello worldhello ", 16, viNormal},
		{"abc", 1, "xp", "acb", 2, viNormal},
		{"abc", 0, "yy$p", "abcabc", 5, viNormal},
		{"ab", 0, "Y2P", "ababab", 3, viNormal},
		{"abc", 0, "p", "abc", 0, viNormal},
		// Insert commands.
		{"abc", 0, "aX\x1b", "aXbc", 1, viNormal},
		{"abc", 0, "Axyz\x1b", "abcxyz", 5, viNormal},
		{"  abc", 4, "I-\x1b", "  -abc", 2, viNormal},
		{"abc", 1, "iX\x1b", "aXbc", 1, viNormal},
		{"abc", 2, "i", "abc", 2, viInsert},
		// Undo and redo.
		{"hello world", 0, "dwu", "hello world", 0, viNormal},
		{"hello world", 0, "dwu\x12", "world", 0, viNormal},
		{"hello world", 0, "dwxuu", "hello world", 0, viNormal},
		{"hello world", 0, "cwbye\x1bu", "hello world", 0, viNormal},
		{"hello", 0, "u", "hello", 0, viNormal},
		// Keys without a meaning change nothing and insert nothing.
		{"hello", 1, "zQ", "hello", 1, viNormal},
	}
	for _, tt := range tests {
		v := newViEditor(nil, nil)
		gotLine, gotPos := runVi(t, v, viNormal, tt.line, tt.pos, tt.keys)
		if gotLine != tt.wantLine || gotPos != tt.wantPos || v.mode != tt.wantMode {
			t.Errorf("%q at %d, keys %q: got %q at %d mode %d, want %q at %d mode %d",
				tt.line, tt.pos, tt.keys, gotLine, gotPos, v.mode,
				tt.wantLine, tt.wantPos, tt.wantMode)
		}
	}
}

// TestViInsertMode checks a new line's insert mode: keys are left to the
// Terminal, Esc enters normal mode one character to the left, and the
// insert is one undo step.
func TestViInsertMode(t *testing.T) {
	v := newViEditor(nil, nil)
	line, pos := runVi(t, v, viInsert, "", 0, ":shoz\x1b")
	if line != ":shoz" || pos != 4 || v.mode != viNormal {
		t.Fatalf("got %q at %d mode %d, want %q at 4 in normal mode", line, pos, v.mode, ":shoz")
	}
	line, pos = runVi(t, v, viNormal, line, pos, "xaw\x1b")
	if line != ":show" || pos != 4 {
		t.Fatalf("got %q at %d, want %q at 4", line, pos, ":show")
	}
	line, _ = runVi(t, v, viNormal, line, pos, "uuu")
	if line != "" {
		t.Fatalf("undo of every step gave %q, want the empty line", line)
	}
	if _, _, ok := v.key("x", 0, '\t'); ok {
		t.Fatal("Tab must be left to the completion in normal mode")
	}
	v.mode = viInsert
	if _, _, ok := v.key("x", 0, 'k'); ok {
		t.Fatal("insert mode must leave text keys to the Terminal")
	}
}

func TestViHistory(t *testing.T) {
	history := viTestHistory{":show", ":help", ":probe"}
	tests := []struct {
		keys, want string
	}{
		{"k", ":show"},
		{"kk", ":help"},
		{"3k", ":probe"},
		{"5k", ":probe"},
		{"kkj", ":show"},
		{"kj", ":dra"},
		{"kjj", ":dra"},
		{"j", ":dra"},
		// x on a recalled line is an undo step of that line.
		{"kxu", ":show"},
	}
	for _, tt := range tests {
		v := newViEditor(history, nil)
		got, _ := runVi(t, v, viNormal, ":dra", 3, tt.keys)
		if got != tt.want {
			t.Errorf("keys %q: got %q, want %q", tt.keys, got, tt.want)
		}
	}
	// A typed line that equals an entry comes back with j past the newest.
	v := newViEditor(history, nil)
	if got, _ := runVi(t, v, viNormal, ":show", 0, "kjj"); got != ":show" {
		t.Errorf("kjj on a line equal to an entry gave %q, want %q", got, ":show")
	}
}

// TestViResetAndModeChanges checks that a new line starts in insert mode
// with no undo steps but keeps the register, and that every mode change is
// reported (for the cursor shape).
func TestViResetAndModeChanges(t *testing.T) {
	var modes []viMode
	v := newViEditor(nil, func(mode viMode) { modes = append(modes, mode) })
	line, pos := runVi(t, v, viInsert, "", 0, "abc\x1byy")
	v.reset()
	if v.mode != viInsert || len(v.undo) != 0 || string(v.register) != "abc" {
		t.Fatalf("after reset: mode %d, %d undo steps, register %q", v.mode, len(v.undo), string(v.register))
	}
	line, _ = runVi(t, v, viNormal, line, pos, "p")
	if line != "abcabc" {
		t.Fatalf("put after reset gave %q", line)
	}
	want := []viMode{viInsert, viNormal, viInsert}
	if len(modes) != len(want) {
		t.Fatalf("mode changes %v, want %v", modes, want)
	}
	for i := range want {
		if modes[i] != want[i] {
			t.Fatalf("mode changes %v, want %v", modes, want)
		}
	}
}

func TestEditInputViEscape(t *testing.T) {
	esc, edit := string(viEscapeKey), string(externalEditKey)
	tests := []struct {
		name, in, want string
		vi             bool
	}{
		{"lone esc", "\x1b", esc, true},
		{"esc esc", "\x1b\x1b", esc + esc, true},
		{"text then esc", "abc\x1b", "abc" + esc, true},
		{"alt-e still the editor", "\x1be", edit, true},
		{"arrow untouched", "\x1b[A", "\x1b[A", true},
		{"esc in paste untouched", "\x1b[200~a\x1b\x1b[201~", "\x1b[200~a\x1b\x1b[201~", true},
		{"emacs mode keeps a lone esc", "\x1b", "\x1b", false},
	}
	for _, tt := range tests {
		in := &editInput{r: strings.NewReader(tt.in), viEscape: tt.vi}
		buf := make([]byte, 256)
		n, err := in.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(buf[:n]); got != tt.want {
			t.Errorf("%s: Read(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

// TestViReviewRegressions covers the cases of the vi mode review: the
// cursor past the line's end (where the Terminal's Up, End and Right put
// it), the register kept by an empty delete, Tab ending a pending operator,
// k continuing from a line recalled with Up, e before trailing blanks, and
// a put count capped at the editor's line limit.
func TestViReviewRegressions(t *testing.T) {
	v := newViEditor(nil, nil)
	v.mode = viNormal
	v.register = []rune("zz")
	if line, pos, ok := v.key("abc", 3, 'p'); !ok || line != "abczz" || pos != 4 {
		t.Fatalf("p past the end: %q at %d (%v), want %q at 4", line, pos, ok, "abczz")
	}
	if line, _ := runVi(t, newViEditor(nil, nil), viNormal, "abc", 3, "x"); line != "ab" {
		t.Fatalf("x past the end gave %q, want %q", line, "ab")
	}
	if line, pos := runVi(t, newViEditor(nil, nil), viNormal, "abc", 0, "yy0Xp"); line != "aabcbc" || pos != 3 {
		t.Fatalf("X at the start cleared the register: %q at %d", line, pos)
	}
	v = newViEditor(nil, nil)
	if line, _ := runVi(t, v, viNormal, "", 0, "xyyp"); line != "" {
		t.Fatalf("x on an empty line put %q", line)
	}
	v = newViEditor(nil, nil)
	if line, pos := runVi(t, v, viNormal, "hello world", 0, "d\tw"); line != "hello world" || pos != 6 {
		t.Fatalf("d Tab w: %q at %d, want the line kept and the cursor at 6", line, pos)
	}
	v = newViEditor(viTestHistory{":show", ":help"}, nil)
	if line, _ := runVi(t, v, viNormal, ":show", 0, "k"); line != ":help" {
		t.Fatalf("k after Up recalled %q, want %q", line, ":help")
	}
	if _, pos := runVi(t, newViEditor(nil, nil), viNormal, "ab  ", 0, "ee"); pos != 1 {
		t.Fatalf("ee before trailing blanks at %d, want 1", pos)
	}
	v = newViEditor(nil, nil)
	line, _ := runVi(t, v, viNormal, "ab", 0, "yy9999p")
	if n := utf8.RuneCountInString(line); n >= editorMaxLine || n < editorMaxLine-3 {
		t.Fatalf("9999p made a line of %d runes, want just below %d", n, editorMaxLine)
	}
}

// TestEditInputBacklogSplitAcrossReads feeds a backlog larger than one
// read: an escape sequence or paste end marker cut at the read boundary is
// held for the next read, not taken for a lone Esc, and a lone Esc at the
// end of the input still is one.
func TestEditInputBacklogSplitAcrossReads(t *testing.T) {
	esc := string(viEscapeKey)
	paste := "\x1b[200~" + strings.Repeat("a", 54) + "\x1b[201~"
	tests := []struct {
		name, in, want string
	}{
		{"held arrow keys", strings.Repeat("\x1b[D", 30), strings.Repeat("\x1b[D", 30)},
		{"paste end cut", paste + "\x1b", paste + esc},
		{"lone esc at a full read", strings.Repeat("x", 63) + "\x1b" + "b", strings.Repeat("x", 63) + "\x1bb"},
	}
	for _, tt := range tests {
		in := &editInput{r: strings.NewReader(tt.in), viEscape: true}
		var got strings.Builder
		buf := make([]byte, 8)
		for {
			n, err := in.Read(buf)
			got.Write(buf[:n])
			if err != nil {
				break
			}
		}
		if got.String() != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got.String(), tt.want)
		}
		if in.pasting {
			t.Errorf("%s: still pasting after the paste end", tt.name)
		}
	}
}

// TestViFableReviewRegressions covers the third review: ; after t/T
// searches past the character next to the cursor, S or cc on an empty
// line keeps the register, and a huge count ends quickly.
func TestViFableReviewRegressions(t *testing.T) {
	tests := []struct {
		line    string
		pos     int
		keys    string
		wantPos int
	}{
		{"a,b,c,d", 0, "t,;", 2},
		{"a,b,c,d", 0, "t,;;", 4},
		{"a,,b", 0, "t,;", 1},
		{"a,b,c,d", 6, "T,;", 4},
		{"a,b,c,d", 0, "t,;,", 2},
	}
	for _, tt := range tests {
		if _, pos := runVi(t, newViEditor(nil, nil), viNormal, tt.line, tt.pos, tt.keys); pos != tt.wantPos {
			t.Errorf("%q at %d, keys %q: cursor at %d, want %d", tt.line, tt.pos, tt.keys, pos, tt.wantPos)
		}
	}
	if line, _ := runVi(t, newViEditor(nil, nil), viNormal, "a,b,c,d", 0, "t,;d;"); line != "a,,d" {
		t.Errorf("d; after t, gave %q, want %q", line, "a,,d")
	}
	for _, keys := range []string{"S\x1bp", "cc\x1bp", "C\x1bp"} {
		v := newViEditor(nil, nil)
		v.register = []rune("abc")
		if line, _ := runVi(t, v, viNormal, "", 0, keys); line != "abc" {
			t.Errorf("%q on an empty line: put %q, want the register kept", keys, line)
		}
	}
	line := strings.Repeat("ab  ", 1000)
	// e stops at the last word's end, so the trailing blanks stay.
	if got, _ := runVi(t, newViEditor(nil, nil), viNormal, line, 0, "9999d9999e"); got != "  " {
		t.Errorf("9999d9999e left %q", got)
	}
}
