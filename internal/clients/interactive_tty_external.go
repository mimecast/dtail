package clients

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"
)

// externalEditKey is the key the Terminal passes to the key callback for
// Alt-E or Alt-V (editInput): edit the line in $VISUAL/$EDITOR, as fish's
// edit_command_buffer does. x/term reads ESC followed by a letter as one
// anonymous unknown key, so the input is translated before the Terminal
// sees it. utf8.MaxRune is valid UTF-8, no key a keyboard sends, and not
// in x/term's private key range, so the Terminal hands it to the callback,
// which consumes it.
const externalEditKey = utf8.MaxRune

// externalEditFallback is run when neither VISUAL nor EDITOR is set.
const externalEditFallback = "vi"

// editInput reads the control TTY for the Terminal and turns the Alt-E and
// Alt-V sequences (ESC 'e', ESC 'v', sent by the terminal in one write)
// into externalEditKey. In the vi editing mode (viEscape) a lone Esc, an
// ESC that ends the input read or is followed by another ESC, becomes
// viEscapeKey: x/term would keep it as the start of a sequence and read it
// with the next key as one unknown key. There the Up and Down arrows
// (ESC [ A, ESC [ B) and Ctrl-P and Ctrl-N, which x/term reads as Up and
// Down, become viUpKey and viDownKey, so that the vi layer recalls history
// instead of the Terminal.
//
// Bracketed paste (ESC [200~ ... ESC [201~, which the editor asks the
// terminal for, interactiveEditor.setPasteMode) is no typing: the Terminal
// inserts pasted keys without the key callback, so nothing in a paste is
// translated (a translated key would land in the line), Tab completes
// nothing and a line break enters the line. Three things are done for it
// here, as the Terminal does none of them:
//   - A control or format character in a paste other than a line break
//     (a tab, an ESC, Ctrl-C, a C1 control, U+202E, U+2028; hiddenRune)
//     becomes a blank (pasteBlank), as in the text of the external editor
//     (editedLine): the Terminal would put it on the line raw (a tab moves
//     the cursor to the next tab stop while the Terminal counts one
//     column) or, for an ESC sequence, as one of its private key codes. A
//     character cut by a read is held back for the next one.
//   - A Read ends after the first line break (chunk), so the Terminal
//     reads every pasted line with a Read of its own.
//   - onPaste is called before a Read returns pasted text that stays on
//     the line (announcePaste): the editor pauses the output for typing,
//     as the first typed key does through the key callback. A pasted line
//     that ends with its line break needs no pause, it runs at once.
//
// Reads are as large as the Terminal's, and a read that filled the buffer
// (more input is queued) holds back a trailing ESC or unfinished paste
// marker for the next one (holdsTail), so a backlog (keys typed while the
// output blocked) is not misread where a read happens to cut it; inside a
// paste, and for a marker cut after more than its ESC, every read does, as
// the rest of the marker is sure to follow. Translated text that does not
// fit the caller's buffer is kept for the next Read. Terminals send a
// key's sequence in one write; only one split by the link itself (ssh over
// a bad line) is read as Esc plus keys, and an Esc typed ahead together
// with the next key is read as Alt plus that key (accepted). An arrow
// split that way on a short read is not translated either: cut after its
// ESC it is Esc plus "[A" typed, cut after "ESC [" the Terminal puts it
// together and recalls at its own history position, which k/j do not
// share (accepted as well). A paste start marker cut right after its ESC
// by a short read is not seen here either, as that ESC alone is the Esc
// key: in the default editing mode the Terminal puts the marker together
// and takes the paste, without the three things above; in the vi editing
// mode the ESC enters normal mode and "[200~" and the pasted text are read
// as normal mode keys (both accepted: a terminal writes the marker in one
// piece, and only a link that splits it there gets this). The markers are
// all there is to tell a paste from typing: pasted text that itself holds
// the end marker (a terminal that does not filter it out of the clipboard)
// ends the paste there, and the rest is read as typed keys.
type editInput struct {
	r        io.Reader
	pasting  bool
	viEscape bool
	// onPaste, when set, is called in the goroutine reading the line
	// before pasted text that stays on the line is returned.
	onPaste func()
	// pasteOut reports whether out holds pasted text.
	pasteOut bool
	// held is input held back from the previous read, out translated
	// input not yet returned, err the read error to return after it.
	held, out []byte
	err       error
}

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
	editKey    = []byte(string(externalEditKey))
	escapeKey  = []byte(string(viEscapeKey))
	arrowUp    = []byte("\x1b[A")
	arrowDown  = []byte("\x1b[B")
	upKey      = []byte(string(viUpKey))
	downKey    = []byte(string(viDownKey))
)

// ctrlP and ctrlN are Ctrl-P and Ctrl-N, which x/term reads as the Up and
// Down arrows; escByte starts every sequence.
const (
	ctrlP   = 0x10
	ctrlN   = 0x0e
	escByte = 0x1b
)

// editInputMinRead is the smallest read of the TTY, also for a caller
// with a smaller buffer.
const editInputMinRead = 64

func (in *editInput) Read(p []byte) (int, error) {
	for len(in.out) == 0 && in.err == nil {
		in.fill(len(p))
	}
	n := in.chunk(len(p))
	in.announcePaste(in.out[:n])
	copy(p, in.out[:n])
	in.out = in.out[n:]
	if len(in.out) == 0 && in.err != nil {
		err := in.err
		in.err = nil
		return n, err
	}
	return n, nil
}

// fill reads the TTY once and translates what it read, behind what the
// previous read held back, into out.
func (in *editInput) fill(size int) {
	buf := make([]byte, max(size, editInputMinRead))
	n, err := in.r.Read(buf)
	data := make([]byte, 0, len(in.held)+n)
	data = append(append(data, in.held...), buf[:n]...)
	in.held = nil
	in.pasteOut = in.pasting
	in.out = in.translate(data, n == len(buf))
	if err != nil {
		// Nothing follows: what was held back is passed on as it is.
		in.out, in.held = append(in.out, in.held...), nil
	}
	in.err = err
}

// chunk returns how many bytes of out the next Read returns: at most
// limit, ending after the first line break (after the LF of a CR LF), so
// that the Terminal reads every line with a Read of its own, and not
// inside a paste marker where the marker can go to the next Read whole.
func (in *editInput) chunk(limit int) int {
	limit = min(limit, len(in.out))
	n := limit
	if end := bytes.IndexAny(in.out[:n], "\r\n"); end >= 0 {
		n = end + 1
		if in.out[end] == '\r' && n < limit && in.out[n] == '\n' {
			n++
		}
	}
	for i := max(n-len(pasteEnd)+1, 1); i < n; i++ {
		rest := in.out[i:]
		if i+len(pasteEnd) > n && (bytes.HasPrefix(rest, pasteStart) || bytes.HasPrefix(rest, pasteEnd)) {
			return i
		}
	}
	return n
}

// announcePaste calls onPaste when chunk, about to be returned, holds
// pasted text that stays on the line: text besides the paste markers that
// does not end with a line break.
func (in *editInput) announcePaste(chunk []byte) {
	if in.onPaste == nil || !in.pasteOut {
		return
	}
	text := bytes.ReplaceAll(bytes.ReplaceAll(chunk, pasteStart, nil), pasteEnd, nil)
	if len(text) > 0 && text[len(text)-1] != '\n' && text[len(text)-1] != '\r' {
		in.onPaste()
	}
}

// holdsTail reports whether rest, the end of a read starting with ESC, is
// held back for the next read: the start of a paste marker (not all of
// one), when the read filled its buffer (full: more input is queued), in a
// paste (its end marker follows for sure) or when it is more than the
// ESC. A lone ESC ending a short read outside a paste is the Esc key.
func (in *editInput) holdsTail(rest []byte, full bool) bool {
	if len(rest) >= len(pasteEnd) || (!bytes.HasPrefix(pasteStart, rest) && !bytes.HasPrefix(pasteEnd, rest)) {
		return false
	}
	return full || in.pasting || len(rest) > 1
}

// translate returns data with every Alt-E and Alt-V outside a bracketed
// paste replaced by externalEditKey and, in the vi editing mode, every
// lone Esc, Up (or Ctrl-P) and Down (or Ctrl-N) outside one by
// viEscapeKey, viUpKey and viDownKey; inside a paste the control
// and format characters become blanks (translateAt). The end of data that
// the next read may continue goes to held (holdsTail; full says that the
// read filled its buffer; in a paste also a character the read cut).
func (in *editInput) translate(data []byte, full bool) []byte {
	special := "\x1b"
	if in.viEscape {
		special = "\x1b\x10\x0e"
	}
	if !in.pasting && !bytes.ContainsAny(data, special) {
		return data
	}
	out := make([]byte, 0, len(data)+len(editKey))
	for i := 0; i < len(data); {
		rest := data[i:]
		if (rest[0] == escByte && in.holdsTail(rest, full)) || (in.pasting && !utf8.FullRune(rest)) {
			in.held = append([]byte(nil), rest...)
			break
		}
		key, used := in.translateAt(rest)
		out = append(out, key...)
		i += used
	}
	return out
}

// translateAt returns what the start of rest is passed on as and how many
// bytes of rest that takes: a paste marker (which starts or ends the
// paste) and any other byte as they are, a control character in a paste as
// a blank, and outside a paste the keys translate names as their key.
func (in *editInput) translateAt(rest []byte) (key []byte, used int) {
	switch {
	case !in.pasting && bytes.HasPrefix(rest, pasteStart):
		in.pasting, in.pasteOut = true, true
		return pasteStart, len(pasteStart)
	case in.pasting && bytes.HasPrefix(rest, pasteEnd):
		in.pasting = false
		return pasteEnd, len(pasteEnd)
	case in.pasting:
		r, size := utf8.DecodeRune(rest)
		if pasteBlank(r) {
			return []byte{' '}, size
		}
		return rest[:size], size
	case len(rest) >= 2 && rest[0] == escByte && (rest[1] == 'e' || rest[1] == 'v'):
		return editKey, 2
	case in.viEscape:
		return viKeyAt(rest)
	}
	return rest[:1], 1
}

// viKeyAt is translateAt for the keys of the vi editing mode outside a
// paste.
func viKeyAt(rest []byte) (key []byte, used int) {
	switch {
	case bytes.HasPrefix(rest, arrowUp):
		return upKey, len(arrowUp)
	case bytes.HasPrefix(rest, arrowDown):
		return downKey, len(arrowDown)
	case rest[0] == ctrlP:
		return upKey, 1
	case rest[0] == ctrlN:
		return downKey, 1
	case rest[0] == escByte && (len(rest) == 1 || rest[1] == escByte):
		return escapeKey, 1
	}
	return rest[:1], 1
}

// pasteBlank reports whether the pasted character r becomes a blank: a
// control or format character (hiddenRune: C0 and C1 controls, DEL,
// U+202E, U+2028, ...) other than a line break. A byte that is no valid
// UTF-8 is passed on as it is.
func pasteBlank(r rune) bool {
	return r != '\n' && r != '\r' && hiddenRune(r)
}

// editorTTY is the Terminal's connection: input through editInput, output
// straight to the TTY.
type editorTTY struct {
	io.Reader
	io.Writer
}

// editExternally runs the external editor on the line being edited
// (Alt-E/Alt-V) and returns the edited line, put on the prompt line to be
// reviewed and entered (not run). The output pauses as for typing. The
// editor gets the TTY in the mode dtail found it in (cbreak mode is set
// again afterwards, also not by the SIGCONT watcher meanwhile). A failed
// editor (not found, or a non-zero exit such as vim's :cq) keeps the line
// and says why; the edited text's line breaks become blanks, as a control
// line is one line (the query language takes them as blanks anyway).
func (c *baseClient) editExternally(e *interactiveEditor, pause *interactivePause,
	line string, pos int) (string, int, bool) {

	if e.answering() {
		return line, pos, true
	}
	e.startTyping(pause)
	edited, err := e.runExternalEditor(line)
	if e.vi != nil {
		// The line comes back with the cursor at its end: go on in insert
		// mode, the editor's change one undo step (and the cursor shape
		// set again, as the editor may have changed it).
		e.vi.enterInsert(snapshot([]rune(line), utf8.RuneCountInString(line[:pos])))
	}
	e.redrawLine()
	if err != nil {
		_, _ = e.terminal.Write([]byte(e.style.hint("editor: "+err.Error()) + "\n"))
		return line, pos, true
	}
	return edited, len(edited), true
}

// runExternalEditor writes line to a temporary file, runs the editor on it
// and returns the file's text as one line.
func (e *interactiveEditor) runExternalEditor(line string) (string, error) {
	file, err := os.CreateTemp("", "dtail-command-*.txt")
	if err != nil {
		return "", err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = file.WriteString(line + "\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = e.startEditing(); err != nil {
		return "", err
	}
	err = e.runEditor(path)
	e.endEditing()
	if err != nil {
		return "", err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return editedLine(string(text))
}

// errEditorSessionEnded reports an Alt-E after the session ended.
var errEditorSessionEnded = errors.New("the session ended")

// startEditing gives the terminal to the external editor in the mode dtail
// found it in, bracketed paste off (the editor sets its own).
func (e *interactiveEditor) startEditing() error {
	e.editMu.Lock()
	defer e.editMu.Unlock()
	if e.closed {
		return errEditorSessionEnded
	}
	if err := e.mode.restore(); err != nil {
		return err
	}
	e.setPasteMode(false)
	e.editing = true
	return nil
}

// endEditing takes the terminal back after the editor: cbreak mode,
// bracketed paste and the window size, unless the session ended (closed)
// while the editor ran.
func (e *interactiveEditor) endEditing() {
	e.editMu.Lock()
	e.editing = false
	closed := e.closed
	if !closed {
		_ = e.mode.reapply()
		e.setPasteMode(true)
	}
	e.editMu.Unlock()
	if !closed && e.resize != nil {
		e.resize()
	}
}

// editedLine turns the text the editor saved into one control line: line
// breaks become blanks and blanks at both ends are dropped.
func editedLine(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errors.New("the edited text is not valid UTF-8, line kept")
	}
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	// Tabs and other control characters become blanks: the Terminal would
	// write them raw and count each as one column.
	joined := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.Join(lines, " "))
	edited := strings.TrimSpace(joined)
	if utf8.RuneCountInString(edited) >= editorMaxLine {
		return "", fmt.Errorf("the edited line is longer than %d characters, line kept",
			editorMaxLine-1)
	}
	return edited, nil
}

// externalEditorCommand returns the editor command: $VISUAL, else $EDITOR,
// else vi, as fish and most tools pick it. It may carry arguments ("code
// -w"), so it is run by the shell.
func externalEditorCommand(getenv func(string) string) string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if command := strings.TrimSpace(getenv(name)); command != "" {
			return command
		}
	}
	return externalEditFallback
}

// runEditorOnTTY runs command on path with the TTY at ttyPath as its
// standard input, output and error (a new descriptor: the control loop's
// stays non-blocking for its own reads), and waits for it.
func runEditorOnTTY(ttyPath, command, path string) error {
	tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = tty.Close() }()
	cmd := exec.Command("/bin/sh", "-c", command+` "$1"`, "dtail-editor", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w, line kept", command, err)
	}
	return nil
}
