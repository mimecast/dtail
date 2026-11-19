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
// into externalEditKey. Bracketed paste (ESC [200~ ... ESC [201~) is passed
// through untranslated: the Terminal inserts pasted keys without the
// callback, so a translated key would land in the line. In the vi editing
// mode (viEscape) a lone Esc, an ESC that ends the input read or is
// followed by another ESC, becomes viEscapeKey: x/term would keep it as the
// start of a sequence and read it with the next key as one unknown key.
//
// Reads are as large as the Terminal's, and a read that filled the buffer
// (more input is queued) holds back a trailing ESC or unfinished paste
// marker for the next one, so a backlog (keys typed while the output
// blocked) is not misread where a read happens to cut it. Translated text
// that does not fit the caller's buffer is kept for the next Read.
// Terminals send a key's sequence in one write; only one split by the link
// itself (ssh over a bad line) is read as Esc plus keys, and an Esc typed
// ahead together with the next key is read as Alt plus that key (accepted).
type editInput struct {
	r        io.Reader
	pasting  bool
	viEscape bool
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
)

// editInputMinRead is the smallest read of the TTY, also for a caller
// with a smaller buffer.
const editInputMinRead = 64

func (in *editInput) Read(p []byte) (int, error) {
	if len(in.out) == 0 && in.err != nil {
		err := in.err
		in.err = nil
		return 0, err
	}
	if len(in.out) == 0 {
		buf := make([]byte, max(len(p), editInputMinRead))
		n, err := in.r.Read(buf)
		data := make([]byte, 0, len(in.held)+n)
		data = append(append(data, in.held...), buf[:n]...)
		in.held = nil
		if n == len(buf) && err == nil {
			data, in.held = splitHeldTail(data)
		}
		in.out = in.translate(data)
		in.err = err
	}
	n := copy(p, in.out)
	in.out = in.out[n:]
	if len(in.out) == 0 && in.err != nil {
		err := in.err
		in.err = nil
		return n, err
	}
	return n, nil
}

// splitHeldTail splits off the end of data that the next read may
// continue: a lone ESC or the start of a paste marker.
func splitHeldTail(data []byte) ([]byte, []byte) {
	from := max(len(data)-len(pasteEnd)+1, 0)
	for i := len(data) - 1; i >= from; i-- {
		if data[i] != 0x1b {
			continue
		}
		tail := data[i:]
		if bytes.HasPrefix(pasteStart, tail) || bytes.HasPrefix(pasteEnd, tail) {
			return data[:i], append([]byte(nil), tail...)
		}
		break
	}
	return data, nil
}

// translate returns data with every Alt-E and Alt-V outside a bracketed
// paste replaced by externalEditKey.
func (in *editInput) translate(data []byte) []byte {
	if bytes.IndexByte(data, 0x1b) < 0 {
		return data
	}
	out := make([]byte, 0, len(data)+len(editKey))
	for i := 0; i < len(data); i++ {
		rest := data[i:]
		switch {
		case bytes.HasPrefix(rest, pasteStart):
			in.pasting = true
		case bytes.HasPrefix(rest, pasteEnd):
			in.pasting = false
		case !in.pasting && len(rest) >= 2 && rest[0] == 0x1b &&
			(rest[1] == 'e' || rest[1] == 'v'):
			out = append(out, editKey...)
			i++
			continue
		case in.viEscape && !in.pasting && rest[0] == 0x1b &&
			(len(rest) == 1 || rest[1] == 0x1b):
			out = append(out, escapeKey...)
			continue
		}
		out = append(out, data[i])
	}
	return out
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
// found it in.
func (e *interactiveEditor) startEditing() error {
	e.editMu.Lock()
	defer e.editMu.Unlock()
	if e.closed {
		return errEditorSessionEnded
	}
	if err := e.mode.restore(); err != nil {
		return err
	}
	e.editing = true
	return nil
}

// endEditing takes the terminal back after the editor: cbreak mode and the
// window size, unless the session ended (closed) while the editor ran.
func (e *interactiveEditor) endEditing() {
	e.editMu.Lock()
	e.editing = false
	closed := e.closed
	if !closed {
		_ = e.mode.reapply()
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
