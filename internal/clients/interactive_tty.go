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

// editorDeps are the line editor's dependencies on the process: what a
// client run takes from the operating system (processEditorDeps) and a test
// replaces per editor. They are an argument of the editor's constructor, so
// that the client itself carries no field that only tests set. A field left
// nil is the process's (withDefaults), so the zero value is a client run's.
type editorDeps struct {
	// setForceExitHook registers or (with nil) clears the editor's terminal
	// restore for the forced exit (signal.SetBeforeForceExit).
	setForceExitHook func(func())
	// getenv looks up VISUAL and EDITOR for the external editor
	// (Alt-E/Alt-V, os.Getenv).
	getenv func(string) string
	// watchSignals calls onContinue for SIGCONT and onResize for SIGWINCH
	// until the returned stop is called (watchTTYSignals). Both signals go
	// to the whole process, so every editor watching them reacts: tests
	// that send none watch none, or the SIGCONT of another test would have
	// their editor write to its terminal.
	watchSignals func(onContinue, onResize func()) (stop func())
}

// processEditorDeps returns the dependencies of a client run: the process's
// forced-exit hook, environment and signals.
func processEditorDeps() editorDeps {
	return editorDeps{}.withDefaults()
}

// withDefaults returns d with every dependency left nil taken from the
// process.
func (d editorDeps) withDefaults() editorDeps {
	if d.setForceExitHook == nil {
		d.setForceExitHook = signal.SetBeforeForceExit
	}
	if d.getenv == nil {
		d.getenv = os.Getenv
	}
	if d.watchSignals == nil {
		d.watchSignals = watchTTYSignals
	}
	return d
}

// newControlLines returns the line editor on tty when tty is a terminal that
// can be switched to cbreak mode, and plain line reading otherwise. answers
// tells the editor while the host key question is asked (nil: never).
func (c *baseClient) newControlLines(tty *os.File, pause *interactivePause,
	answers *promptAnswers, deps editorDeps) controlLines {

	if editor := c.newInteractiveEditor(tty, pause, answers.prompting, deps); editor != nil {
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
	// foreground reports whether dtail may change the terminal: not while
	// it runs in the background, where the shell owns it (reapply and
	// restore do nothing then either).
	foreground() bool
}

// editorMaxLine is the longest control line the editor takes: x/term's
// Terminal silently stops adding keys at 4096 runes, so a line that long may
// have been cut and is rejected instead of being run.
const editorMaxLine = 4096

// interactiveEditor edits control lines with golang.org/x/term's Terminal in
// cbreak mode (enterCbreakMode): Tab completes (completeEditorLine: a
// --files value's remote paths first, else completeInteractive),
// Up/Down recall earlier lines (in vi mode through the vi layer, which
// shares their history position with k/j), and typing a command pauses
// the output (interactivePause.pauseTyping), so log lines do not run over the line
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
	// hideMu guards hidden and resizePending, and serializes everything
	// that draws the line (hideLine, showLine, the resize and the control
	// output of another goroutine, editorOutput) around them.
	hideMu sync.Mutex
	// hidden is set while a Tab's wait has the line off the screen
	// (hideLine until showLine): the Terminal's cursor bookkeeping still
	// describes the line as drawn, so nothing may have it draw meanwhile.
	hidden bool
	// resizePending is a window size change that arrived while hidden;
	// showLine takes it over once the line is drawn again.
	resizePending bool
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

// newInteractiveEditor returns the line editor on tty, switched to cbreak
// mode, or nil where that mode cannot be set (then plain lines are read).
// The forced-exit hook is registered last: restoreForForcedExit reads the
// editor's fields (mode, vi, screen), which a forced exit running in
// another goroutine must find all set.
func (c *baseClient) newInteractiveEditor(tty *os.File, pause *interactivePause,
	prompting func() bool, deps editorDeps) *interactiveEditor {

	deps = deps.withDefaults()
	ttyFd, mode, err := cbreakOn(tty)
	if err != nil {
		c.clientLogger().Debug("Interactive line editing unavailable, reading plain lines", err)
		return nil
	}
	vi := c.editMode() == config.EditModeVi
	keys := &editInput{r: tty, viEscape: vi}
	input := editorTTY{Reader: keys, Writer: tty}
	editor := &interactiveEditor{terminal: term.NewTerminal(input, ""), screen: newEditorScreen(tty),
		style: c.interactiveStyle(), pause: pause, mode: mode, setForceExitHook: deps.setForceExitHook,
		prompting: prompting}
	editor.runEditor = func(path string) error {
		return runEditorOnTTY(tty.Name(), externalEditorCommand(deps.getenv), path)
	}
	editor.bindKeys(c, pause, vi, tty)
	// Pasted text does not reach the key callback: it pauses the output
	// here, before the Terminal puts it on the line (editInput.onPaste).
	keys.onPaste = func() { editor.startTyping(pause) }
	editor.watchTerminal(ttyFd, deps.watchSignals)
	editor.setPasteMode(true)
	// A forced exit (a second Ctrl-C while the shutdown hangs) calls
	// os.Exit, which skips the deferred close: restore the mode there too,
	// without waiting on anything (restoreForForcedExit).
	editor.setForceExitHook(editor.restoreForForcedExit)
	return editor
}

// cbreakOn switches tty to cbreak mode (enterCbreakMode) and returns its
// file descriptor and the mode to reapply and restore.
func cbreakOn(tty *os.File) (int, ttyMode, error) {
	// SyscallConn instead of Fd: Fd would switch tty to blocking mode, and
	// then closing it (on cancellation) would no longer end a pending read.
	raw, err := tty.SyscallConn()
	if err != nil {
		return 0, nil, err
	}
	var mode ttyMode
	var ttyFd int
	ctlErr := raw.Control(func(fd uintptr) {
		ttyFd = int(fd)
		mode, err = enterCbreakMode(ttyFd)
	})
	return ttyFd, mode, errors.Join(ctlErr, err)
}

// watchTerminal takes over the window size of the terminal ttyFd and
// follows its signals (watch: watchTTYSignals for a client run, stopped by
// close): SIGWINCH resizes, SIGCONT sets the editor's mode again.
func (e *interactiveEditor) watchTerminal(ttyFd int, watch func(onContinue, onResize func()) func()) {
	e.resize = func() {
		// A terminal without a window size (a bare pseudo terminal) reports
		// 0x0; keep the Terminal's 80x24 default then, as width 0 would wrap
		// after every character. Not while the external editor runs:
		// SetSize redraws the line, into the editor's screen. Nor while a
		// Tab's wait has the line off the screen (hidden): SetSize would
		// redraw it from the cursor position the Terminal remembers, over
		// the output that ran since; showLine calls resize again.
		if e.inEditor() {
			return
		}
		e.hideMu.Lock()
		defer e.hideMu.Unlock()
		if e.hidden {
			e.resizePending = true
			return
		}
		if width, height, err := term.GetSize(ttyFd); err == nil && width > 0 && height > 0 {
			_ = e.terminal.SetSize(width, height)
			e.screen.setWidth(width)
		}
	}
	e.resize()
	// After Ctrl-Z and fg, bash and zsh leave the terminal cooked and with
	// their own bracketed paste setting (off while a job runs): set cbreak
	// mode and bracketed paste again (not while the external editor owns
	// the terminal). A resized window changes where lines wrap.
	e.stop = watch(func() {
		e.editMu.Lock()
		defer e.editMu.Unlock()
		if !e.editing && !e.closed {
			_ = e.mode.reapply()
			e.setPasteMode(true)
		}
	}, e.resize)
}

// bindKeys gives the Terminal its history, its key callback (editorKey)
// and, for the vi editing mode, the vi layer, which shows its mode with
// the cursor shape written to out.
func (e *interactiveEditor) bindKeys(c *baseClient, pause *interactivePause, vi bool, out io.Writer) {
	// Recalling a line (Up/Down) is typing too, but x/term does not pass
	// those keys to the callback (in vi mode the vi layer takes them,
	// viUpKey, and editorKey leaves the pause to the history as well): the
	// history reports the recall.
	// The answer to the host key question is no control line to recall.
	history := &controlHistory{onRecall: func() { e.startTyping(pause) }, skip: e.answering}
	e.terminal.History = history
	if vi {
		e.vi = newViEditor(history, func(mode viMode) {
			_, _ = io.WriteString(out, viCursorShapes[mode])
		})
	}
	e.terminal.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		return c.editorKey(e, pause, line, pos, key)
	}
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
			// typing: it pauses the output like an inserted key. Not Up
			// and Down: the history reports their recall (bindKeys), and
			// an arrow that recalled nothing changes no line, like the
			// unbound keys below.
			if key != viUpKey && key != viDownKey {
				e.startTyping(pause)
			}
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
	line, err := e.readPastedOrTyped()
	if err == nil && utf8.RuneCountInString(line) >= editorMaxLine {
		return "", errControlLineTooLong
	}
	return line, err
}

// readPastedOrTyped reads the next line from the Terminal. A pasted line
// (bracketed paste, setPasteMode; the Terminal marks a line that was
// pasted as a whole with ErrPasteIndicator) is a line like any other,
// except a blank one, which is skipped: an empty line toggles the output
// pause, which is meant for the Enter key, not for the blank lines of
// pasted text (between commands, or its first or last line). A blank line
// that was typed, also one entered after pasted blanks, is returned.
func (e *interactiveEditor) readPastedOrTyped() (string, error) {
	for {
		line, err := e.terminal.ReadLine()
		if !errors.Is(err, term.ErrPasteIndicator) {
			return line, err
		}
		if strings.TrimSpace(line) != "" {
			return line, nil
		}
	}
}

func (e *interactiveEditor) Writer() io.Writer {
	return editorWriter{Writer: editorOutput{editor: e}, styled: e.style}
}

// editorOutput is where the editor's control output goes: to the Terminal,
// which takes the line being edited off the screen, writes and draws the
// line again below. The control loop writes its answers between two lines,
// but the Ctrl-C statistics (stats.setInterruptOutput) come from another
// goroutine at any time, also while a Tab's wait has the line off the
// screen (interactiveEditor.hidden): then the Terminal would move up from
// where it believes the cursor is, into the output that ran since, and
// draw the line there. So such a write goes to the TTY as it is, like the
// output running at that time, and showLine draws the line below it.
type editorOutput struct{ editor *interactiveEditor }

func (o editorOutput) Write(p []byte) (int, error) {
	e := o.editor
	e.hideMu.Lock()
	defer e.hideMu.Unlock()
	if e.hidden && e.screen != nil && e.screen.out != nil {
		return e.screen.out.Write(p)
	}
	return e.terminal.Write(p)
}

// Bracketed paste mode (DECSET 2004): the terminal sends pasted text
// between the markers pasteStart and pasteEnd.
const (
	pasteModeOn  = "\x1b[?2004h"
	pasteModeOff = "\x1b[?2004l"
)

// setPasteMode asks the terminal to bracket pasted text with markers, or
// not to any more. With them the Terminal takes a paste as text: a pasted
// tab completes nothing, the lines of a multi-line paste are entered one by
// one as they are, and editInput blanks the paste's control characters and
// pauses the output for pasted text (onPaste). It is on while the editor
// has the terminal: set at the start and whenever cbreak mode is set again
// (after the external editor and after SIGCONT, as shells switch it off
// for the jobs they run), and off for the external editor, on close and on
// the forced exit. Like the terminal mode, it is left alone while dtail
// runs in the background (ttyMode.foreground): the terminal is the
// shell's then, e.g. at the SIGCONT of "bg". A terminal that does not know
// the mode ignores the request and pastes as typed keys, as before. While
// dtail is stopped (Ctrl-Z) the mode stays on, as the terminal mode does:
// a shell that neither sets nor clears it (dash) then shows the markers
// around text pasted at its prompt.
func (e *interactiveEditor) setPasteMode(on bool) {
	if e.screen == nil || e.screen.out == nil || (e.mode != nil && !e.mode.foreground()) {
		return
	}
	sequence := pasteModeOff
	if on {
		sequence = pasteModeOn
	}
	_, _ = io.WriteString(e.screen.out, sequence)
}

// close stops the SIGCONT/SIGWINCH watcher, waiting for it so that no
// reapply runs after the restore, restores the terminal mode (and switches
// bracketed paste off) and clears the forced-exit hook.
func (e *interactiveEditor) close() {
	e.once.Do(func() {
		e.stop()
		e.editMu.Lock()
		e.closed = true
		if !e.editing {
			if e.vi != nil {
				_, _ = e.screen.out.Write([]byte(viCursorDefault))
			}
			e.setPasteMode(false)
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
	// Bracketed paste off and, in vi mode, the default cursor shape back,
	// best effort, in a goroutine that the exit does not wait for: unlike
	// the mode's ioctl these are writes to the terminal, which block while
	// its output is stopped (Ctrl-S), and the forced exit is the way out of
	// a shutdown that hangs. So after a forced exit the terminal may be
	// left with bracketed paste on (and the vi cursor shape): then the
	// shell's own setting applies at its next prompt, or "printf
	// '\033[?2004l'" switches it off.
	vi := e.vi != nil
	go func() {
		e.setPasteMode(false)
		if vi {
			_, _ = e.screen.out.Write([]byte(viCursorDefault))
		}
	}()
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
