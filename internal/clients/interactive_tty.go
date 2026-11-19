package clients

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/signal"
)

// controlHistoryMax bounds the remembered control lines.
const controlHistoryMax = 100

// controlLines is where the interactive control loop reads its lines from
// and writes its answers to: the line editor on a terminal
// (interactiveEditor) or, where the control TTY cannot be switched to cbreak
// mode, plain lines (scannerLines).
type controlLines interface {
	// ReadLine returns the next line without its line ending, or io.EOF at
	// the end of the input.
	ReadLine() (string, error)
	// Writer is where control output goes. The editor's writer clears the
	// line being edited, writes, and draws the line again.
	Writer() io.Writer
	// close restores the terminal mode. It is idempotent and safe to call
	// from another goroutine than the reader's.
	close()
}

// newControlLines returns the line editor on tty when tty is a terminal that
// can be switched to cbreak mode, and plain line reading otherwise. answers
// tells the editor while the host key question is asked (nil: never).
func (c *baseClient) newControlLines(tty *os.File, pause *interactivePause,
	answers *promptAnswers) controlLines {

	if editor := c.newInteractiveEditor(tty, pause, answers.prompting); editor != nil {
		return editor
	}
	scanner := bufio.NewScanner(tty)
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
	return &scannerLines{scanner: scanner, out: tty}
}

// scannerLines reads whole lines as the terminal's line discipline hands them
// over: no completion, history or pause while typing.
type scannerLines struct {
	scanner *bufio.Scanner
	out     io.Writer
}

func (s *scannerLines) ReadLine() (string, error) {
	if s.scanner.Scan() {
		return s.scanner.Text(), nil
	}
	if err := s.scanner.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

func (s *scannerLines) Writer() io.Writer { return s.out }
func (*scannerLines) close()              {}

// ttyMode is the terminal mode the line editor sets (cbreakTTY).
type ttyMode interface {
	reapply() error
	restore() error
}

// editorMaxLine is the longest control line the editor takes: x/term's
// Terminal silently stops adding keys at 4096 runes, so a line that long may
// have been cut and is rejected instead of being run.
const editorMaxLine = 4096

// interactiveEditor edits control lines with golang.org/x/term's Terminal in
// cbreak mode (enterCbreakMode): Tab completes (completeEditorLine: a
// --files value's remote paths first, else completeInteractive),
// Up/Down recall earlier lines, and typing a command pauses the output
// (interactivePause.pauseTyping), so log lines do not run over the line
// being edited. A line started while the output is paused shows
// interactivePrompt; one started while it runs none, as streaming output
// and a prompt would overwrite each other (see ReadLine). While the host key
// question is asked, the line typed is its answer: its keys still pause the
// output, but show no typing hint, complete nothing and do not enter the
// history (answering).
type interactiveEditor struct {
	terminal *term.Terminal
	// pause is the output pause of the control loop, which decides the
	// prompt of every line.
	pause *interactivePause
	// style paints the control output, the prompt and the candidate lists;
	// nil paints nothing (colors disabled).
	style *interactiveStyle
	// screen writes to the TTY next to the Terminal while a Tab lets the
	// output run (waitForListing); nil writes nothing.
	screen *editorScreen
	mode   ttyMode
	stop   func()
	// prompting reports whether the host key question is asked; nil means
	// never (no question can be routed to the control loop).
	prompting func() bool
	// setForceExitHook registers or (with nil) clears the restore for the
	// forced exit.
	setForceExitHook func(func())
	once             sync.Once
	// runEditor runs the external editor on a file (Alt-E/Alt-V,
	// editExternally) with the TTY in the mode dtail found it in.
	runEditor func(path string) error
	// editMu guards editing and closed. While the external editor runs
	// (editing), the terminal belongs to it: the SIGCONT watcher sets no
	// mode, a resize redraws nothing, and a close (the session ending
	// meanwhile) leaves mode and cursor shape alone, as the editor gives
	// the terminal back in the mode it got it in; once closed, the end of
	// the editor sets no mode either.
	editMu  sync.Mutex
	editing bool
	closed  bool
	// resize takes the window size over (SIGWINCH, and after the editor).
	resize func()
	// vi is the vi editing mode (Client.EditMode "vi"); nil edits with
	// x/term's emacs-like keys only.
	vi *viEditor
	// itemBlank is the blank the last Tab inserted after a finished query
	// item, which a comma typed right after it replaces
	// (commaAfterItemBlank); only the key callback, in the goroutine
	// reading the line, uses it.
	itemBlank itemBlank
}

// viCursorShapes are the cursor shapes (DECSCUSR) of the vi modes, as vi
// shells show them: a bar in insert mode, a block in normal mode.
// viCursorDefault gives the terminal its default shape back.
var viCursorShapes = map[viMode]string{viInsert: "\x1b[6 q", viNormal: "\x1b[2 q"}

const viCursorDefault = "\x1b[0 q"

func (c *baseClient) newInteractiveEditor(tty *os.File, pause *interactivePause,
	prompting func() bool) *interactiveEditor {
	// SyscallConn instead of Fd: Fd would switch tty to blocking mode, and
	// then closing it (on cancellation) would no longer end a pending read.
	raw, err := tty.SyscallConn()
	if err != nil {
		return nil
	}
	var mode ttyMode
	var ttyFd int
	ctlErr := raw.Control(func(fd uintptr) {
		ttyFd = int(fd)
		mode, err = enterCbreakMode(ttyFd)
	})
	if ctlErr != nil || err != nil {
		c.clientLogger().Debug("Interactive line editing unavailable, reading plain lines", ctlErr, err)
		return nil
	}
	vi := c.editMode() == config.EditModeVi
	input := editorTTY{Reader: &editInput{r: tty, viEscape: vi}, Writer: tty}
	editor := &interactiveEditor{terminal: term.NewTerminal(input, ""), screen: newEditorScreen(tty),
		style: c.interactiveStyle(), pause: pause, mode: mode, setForceExitHook: c.setForceExitHook,
		prompting: prompting}
	if editor.setForceExitHook == nil {
		editor.setForceExitHook = signal.SetBeforeForceExit
	}
	getenv := c.editorEnv
	if getenv == nil {
		getenv = os.Getenv
	}
	editor.runEditor = func(path string) error {
		return runEditorOnTTY(tty.Name(), externalEditorCommand(getenv), path)
	}
	// A forced exit (a second Ctrl-C while the shutdown hangs) calls
	// os.Exit, which skips the deferred close: restore the mode there too,
	// without waiting on anything (restoreForForcedExit).
	editor.setForceExitHook(editor.restoreForForcedExit)
	// Recalling a line (Up/Down) is typing too, but x/term does not pass
	// those keys to the callback: the history reports the recall.
	// The answer to the host key question is no control line to recall.
	editor.terminal.History = &controlHistory{onRecall: func() { editor.startTyping(pause) },
		skip: editor.answering}
	if vi {
		history, _ := editor.terminal.History.(viHistory)
		editor.vi = newViEditor(history, func(mode viMode) {
			_, _ = io.WriteString(tty, viCursorShapes[mode])
		})
	}
	editor.terminal.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		return c.editorKey(editor, pause, line, pos, key)
	}
	editor.resize = func() {
		// A terminal without a window size (a bare pseudo terminal) reports
		// 0x0; keep the Terminal's 80x24 default then, as width 0 would wrap
		// after every character. Not while the external editor runs:
		// SetSize redraws the line, into the editor's screen.
		if editor.inEditor() {
			return
		}
		if width, height, err := term.GetSize(ttyFd); err == nil && width > 0 && height > 0 {
			_ = editor.terminal.SetSize(width, height)
			editor.screen.setWidth(width)
		}
	}
	editor.resize()
	// After Ctrl-Z and fg, bash and zsh leave the terminal cooked: set
	// cbreak mode again (not while the external editor owns the terminal).
	// A resized window changes where lines wrap.
	editor.stop = watchTTYSignals(func() {
		editor.editMu.Lock()
		defer editor.editMu.Unlock()
		if !editor.editing && !editor.closed {
			_ = mode.reapply()
		}
	}, editor.resize)
	return editor
}

// editorKey is called by the Terminal for the keys it does not handle
// itself: the ones it inserts into the line, Tab, Alt-E/Alt-V
// (externalEditKey, editExternally), and unbound control keys and escape
// sequences. A key that inserts text (or Tab) pauses the output
// for typing; unbound keys (PageUp, F-keys, Ctrl-G) change nothing on the
// line and must not freeze the output unnoticed. Tab completes
// (completeEditorLine, which may wait briefly for a path listing) and is
// consumed; a comma typed right after the blank Tab inserted after a
// finished query item replaces that blank (commaAfterItemBlank, not in vi
// normal mode, where the comma is a motion); all other keys are left to
// the Terminal. While the host key question is asked, Tab is consumed
// without completing: the line is its answer, not a command.
func (c *baseClient) editorKey(e *interactiveEditor, pause *interactivePause,
	line string, pos int, key rune) (string, int, bool) {

	blank := e.takeItemBlank()
	if key == externalEditKey {
		return c.editExternally(e, pause, line, pos)
	}
	if e.vi != nil {
		if viLine, viPos, ok := e.vi.key(line, pos, key); ok {
			// Every key of normal mode, and the Esc that enters it, is
			// typing: it pauses the output like an inserted key.
			e.startTyping(pause)
			return viLine, viPos, true
		}
	}
	if key != '\t' && !insertsText(key) {
		return "", 0, false
	}
	e.startTyping(pause)
	if key != '\t' {
		return blank.commaAfterItemBlank(line, pos, key)
	}
	if e.answering() {
		return line, pos, true
	}
	result := c.completeEditorLine(e, pause, line, pos)
	e.rememberItemBlank(result)
	// x/term releases its lock around this callback, so writing here is
	// safe: Write clears the line, prints the listing (header, heading,
	// candidate groups wrapped to the terminal width, hint) and redraws it.
	listing := e.style.listing(result, e.screen.columns())
	if listing != "" {
		_, _ = e.terminal.Write([]byte(listing + "\n"))
	}
	return result.line, result.pos, true
}

// startTyping pauses the output for typing and, when that starts the pause,
// shows interactiveTypingHint above the line: that the output paused, and
// that Tab completes and :help helps. x/term releases its lock around the
// key callback and the history recall, so Write is safe there; it clears
// the line, prints the hint and draws the line again (with the prompt the
// pause set). While the host key question is asked it only pauses: the
// hint would be written right behind the question (which the Terminal does
// not know of) and push the answer being typed to a later row.
func (e *interactiveEditor) startTyping(pause *interactivePause) {
	if pause.pauseTyping() && !e.answering() {
		_, _ = e.terminal.Write([]byte(e.style.hint(interactiveTypingHint) + "\n"))
	}
}

// answering reports whether the line being typed answers the host key
// question.
func (e *interactiveEditor) answering() bool {
	return e.prompting != nil && e.prompting()
}

// setPrompt sets the prompt of the next line: interactivePrompt while the
// output is paused (explicitly, or kept paused after :help and :fields),
// none while it runs; unpainted on a terminal narrower than the painted
// prompt has runes (interactiveStyle.editorPrompt, with the width the
// Terminal was last told, editorScreen.columns). The Terminal draws its
// prompt where a line starts and counts it in every cursor move after, so
// it is set only between lines: a pause for typing starts with the first
// key of a line, which keeps the prompt it started with, and so does a
// line during which the window is resized. It runs in the control loop
// goroutine, which owns the pause state.
func (e *interactiveEditor) setPrompt() {
	prompt := e.style.editorPrompt(e.pause != nil && e.pause.paused, e.screen.columns())
	e.terminal.SetPrompt(prompt)
	e.screen.setPromptWidth(utf8.RuneCountInString(stripANSI(prompt)))
}

// insertsText reports whether the Terminal adds key to the line, as its own
// isPrintable decides: runes from the space up, except its private key codes
// for special keys (0xd800 and up, the surrogate area).
func insertsText(key rune) bool {
	return key >= 32 && (key < 0xd800 || key > 0xdbff)
}

// errControlLineTooLong reports a control line of editorMaxLine runes or
// more, which the Terminal may have cut. The control loop reports it, does
// not run it, and resumes the output its keys paused for typing.
var errControlLineTooLong = fmt.Errorf(
	"line too long (the line editor takes up to %d characters), ignored", editorMaxLine-1)

// ReadLine returns the next control line, or errControlLineTooLong for a
// line the Terminal may have cut.
func (e *interactiveEditor) ReadLine() (string, error) {
	e.setPrompt()
	e.itemBlank = itemBlank{}
	if e.vi != nil {
		e.vi.reset()
	}
	line, err := e.terminal.ReadLine()
	if errors.Is(err, term.ErrPasteIndicator) {
		// A pasted line (bracketed paste) is a line like any other.
		err = nil
	}
	if err == nil && utf8.RuneCountInString(line) >= editorMaxLine {
		return "", errControlLineTooLong
	}
	return line, err
}

func (e *interactiveEditor) Writer() io.Writer {
	return editorWriter{Writer: e.terminal, styled: e.style}
}

// close stops the SIGCONT/SIGWINCH watcher, waiting for it so that no
// reapply runs after the restore, restores the terminal mode and clears the
// forced-exit hook.
func (e *interactiveEditor) close() {
	e.once.Do(func() {
		e.stop()
		e.editMu.Lock()
		e.closed = true
		if !e.editing {
			if e.vi != nil {
				_, _ = e.screen.out.Write([]byte(viCursorDefault))
			}
			_ = e.mode.restore()
		}
		e.editMu.Unlock()
		e.setForceExitHook(nil)
	})
}

// restoreForForcedExit is the forced exit's hook: it only restores the
// terminal mode, right before os.Exit. It must return whatever hangs, as the
// forced exit is the way out of a hung shutdown, so it neither shares close's
// sync.Once (a close in progress may itself be waiting) nor waits for the
// watcher: the watcher's resize (Terminal.SetSize) needs x/term's lock, which
// a Terminal.Write blocked on stopped terminal output (Ctrl-S) holds.
// restore itself does not wait for output to drain (TCSETS/TIOCSETA). The
// watcher keeps running, so only a SIGCONT handled in the moment between
// this restore and os.Exit could set cbreak mode again.
func (e *interactiveEditor) restoreForForcedExit() {
	_ = e.mode.restore()
	if e.vi != nil {
		// The default cursor shape back, best effort: a write to stopped
		// terminal output would block, and the exit must not wait for it.
		go func() { _, _ = e.screen.out.Write([]byte(viCursorDefault)) }()
	}
}

// inEditor reports whether the external editor runs.
func (e *interactiveEditor) inEditor() bool {
	e.editMu.Lock()
	defer e.editMu.Unlock()
	return e.editing
}

// controlHistory remembers the control lines for Up/Down, most recent first,
// without blank lines (which pause and resume the output), without
// repeating the latest line and without the lines skip reports (answers to
// the host key question).
type controlHistory struct {
	entries []string
	// onRecall is called when the Terminal recalls an entry (Up/Down).
	onRecall func()
	// skip, when set, is asked as a line is entered whether to drop it. The
	// Terminal adds the line before ReadLine returns it, so the host key
	// question is still asked then if the line answers it.
	skip func() bool
}

func (h *controlHistory) Add(entry string) {
	// A line the Terminal may have cut (errControlLineTooLong) is not kept.
	if (h.skip != nil && h.skip()) || strings.TrimSpace(entry) == "" || (len(h.entries) > 0 && h.entries[0] == entry) ||
		utf8.RuneCountInString(entry) >= editorMaxLine {
		return
	}
	h.entries = append([]string{entry}, h.entries...)
	if len(h.entries) > controlHistoryMax {
		h.entries = h.entries[:controlHistoryMax]
	}
}

func (h *controlHistory) Len() int { return len(h.entries) }
func (h *controlHistory) At(idx int) string {
	if h.onRecall != nil {
		h.onRecall()
	}
	return h.entries[idx]
}

func trimEach(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = strings.TrimSpace(w)
	}
	return out
}

// editMode returns the line editor's editing mode (Client.EditMode,
// --edit-mode): config.EditModeVi or the default emacs-like keys.
func (c *baseClient) editMode() string {
	if c.runtime == nil || c.runtime.clientCfg == nil {
		return config.EditModeEmacs
	}
	return c.runtime.clientCfg.EditMode
}
