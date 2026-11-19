package clients

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// editorDefaultWidth is the width golang.org/x/term's Terminal assumes
// until it learns the window size (SetSize).
const editorDefaultWidth = 80

// pathListingMissingHint is shown when a Tab gave up waiting for a path
// listing: the request goes on, and a later Tab uses its answer.
const pathListingMissingHint = "(no listing of %s from the server within %s: press Tab again to use it once it arrives)"

// tableProbeMissingHint is shown when a Tab completed a query from the
// cache while the table probe of its from table had not answered yet.
const tableProbeMissingHint = "(the servers have not answered the probe of table %s yet: " +
	"press Tab again to use it once it arrives)"

// completeEditorLine completes the line being edited at a Tab press: a
// --files value from the path listings (completeFilesValue, waiting for a
// listing that is not cached, awaitPathListing), anything else with
// completeInteractive and, for a query naming a from table,
// completeWithTableProbe.
func (c *baseClient) completeEditorLine(e *interactiveEditor, pause *interactivePause,
	line string, pos int) completion {

	args, _ := c.snapshotConnectionState()
	result, ok := completeFilesValue(line, pos, args, c.prober.pathListingSource())
	if !ok {
		// A method value on a nil prober is fine: catalogSnapshot accepts nil.
		result = completeInteractive(line, pos, args, c.prober.catalogSnapshot)
		if result.probeTable != "" {
			result = c.completeWithTableProbe(e, pause, line, pos, result)
		}
		return result
	}
	if result.fetch == "" {
		return result
	}
	return c.awaitPathListing(e, pause, line, pos, result)
}

// completeWithTableProbe has the prober ask for a table probe of the from
// table of a query completion (requestTable; nothing is sent while a fresh
// answer is cached or a failed probe of it is recent) and, only for the
// Tab that sent the request, waits for it like a path listing, at most the
// prober's listWait with the output running (waitForListing), then
// completes the line again. A later Tab while the probe is still on its way
// does not wait again: it completes from the cache as result does, with
// tableProbeMissingHint, as does the Tab whose wait ran out.
func (c *baseClient) completeWithTableProbe(e *interactiveEditor, pause *interactivePause,
	line string, pos int, result completion) completion {

	done, started := c.prober.requestTable(result.probeTable)
	if done == nil {
		return result
	}
	if started && c.waitForListing(e, pause, line, pos, done, c.prober.listWait) {
		args, _ := c.snapshotConnectionState()
		return completeInteractive(line, pos, args, c.prober.catalogSnapshot)
	}
	result.hint = joinLines(result.hint, fmt.Sprintf(tableProbeMissingHint, displayText(result.probeTable)))
	return result
}

// awaitPathListing sends the listing request result asks for
// (interactiveProber.listPaths) and waits for it, at most the prober's
// listWait in total, then completes again. A truncated listing can ask for
// a narrower one, which is waited for within the same time. When no
// listing arrived in time (or the server could not answer, e.g. because
// another probe of the session still ran there), the candidates found so
// far are kept and a hint says so; the request goes on, and the next Tab
// uses a late answer from the cache.
func (c *baseClient) awaitPathListing(e *interactiveEditor, pause *interactivePause,
	line string, pos int, result completion) completion {

	wait := pathListingWait
	if c.prober != nil {
		wait = c.prober.listWait
	}
	deadline := time.Now().Add(wait)
	for result.fetch != "" {
		done := c.prober.listPaths(result.fetch)
		remaining := time.Until(deadline)
		if done == nil || remaining <= 0 || !c.waitForListing(e, pause, line, pos, done, remaining) {
			break
		}
		args, _ := c.snapshotConnectionState()
		next, _ := completeFilesValue(line, pos, args, c.prober.pathListingSource())
		answered := next.fetch != result.fetch
		result = next
		if !answered {
			// The request ended without a listing to cache.
			break
		}
	}
	if result.fetch != "" {
		dir, _ := listingDirKey(result.fetch)
		result.hint = joinLines(result.hint, fmt.Sprintf(pathListingMissingHint, dir, wait))
		result.fetch = ""
	}
	return result
}

// waitForListing waits until done (a path listing or table probe request,
// listPaths or requestTable) is closed or timeout passed and reports
// whether it was closed. A paused output runs meanwhile
// (interactivePause.runOutput): with a remote server the connection handler
// reads the answer only while it can write the output, and in serverless
// mode the in-process server handles the request only while it can write
// its log lines, which go to the paused client output as well (so the
// serverless answer does not arrive while paused either, although its
// payload bypasses the handler, see TestProbeAnswerWhileOutputPaused). The
// line being edited is taken off the screen first, so the output does not
// run into it, and the Terminal draws it again afterwards (editorScreen).
func (c *baseClient) waitForListing(e *interactiveEditor, pause *interactivePause,
	line string, pos int, done <-chan struct{}, timeout time.Duration) bool {

	wait := func() bool {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			return true
		case <-timer.C:
			return false
		}
	}
	if !pause.paused {
		return wait()
	}
	var arrived bool
	e.screen.hideLine(line, pos)
	pause.runOutput(func() { arrived = wait() })
	e.screen.restoreLine(line, pos)
	e.redrawLine()
	return arrived
}

// redrawLine has the Terminal draw its prompt and line again below the
// output a Tab let run (after restoreLine). Its Write clears the rows it
// believes the line takes and draws prompt and line, which also puts its
// cursor bookkeeping right again: hideLine erased the prompt too, and
// without a redraw (a completion that lists nothing only sets the new
// line, starting at the prompt's end) the line would be drawn without the
// prompt where the Terminal believes the prompt is, misplacing every later
// cursor move by its width. x/term releases its lock around the key
// callback, so Write is safe here.
func (e *interactiveEditor) redrawLine() {
	if e != nil && e.terminal != nil {
		_, _ = e.terminal.Write(nil)
	}
}

// joinLines joins the non-empty lines with line breaks.
func joinLines(lines ...string) string {
	var kept []string
	for _, line := range lines {
		if line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// editorScreen writes to the control TTY next to the Terminal, to let the
// output run while a line is being edited (waitForListing). The Terminal
// moves its cursor only relative to where it believes it is: in the row
// of the line being edited at the cursor's position. So the line is erased
// before the output runs (hideLine), and afterwards (restoreLine) the
// cursor is put as many rows below the output as the Terminal will move
// up, so that its next redraw lands below the output instead of over it.
// Like the Terminal, it counts one column per rune (of the prompt, too)
// and wraps at the width it was last told (setWidth). A nil screen, or one
// without a writer, writes nothing.
type editorScreen struct {
	out   io.Writer
	width atomic.Int64
	// promptWidth is the columns of the Terminal's prompt in front of the
	// line (setPromptWidth).
	promptWidth atomic.Int64
}

func newEditorScreen(out io.Writer) *editorScreen {
	s := &editorScreen{out: out}
	s.width.Store(editorDefaultWidth)
	return s
}

// setWidth records the terminal width the Terminal was told (SetSize).
func (s *editorScreen) setWidth(width int) {
	if s != nil && width > 0 {
		s.width.Store(int64(width))
	}
}

// columns returns the terminal width the Terminal was last told (setWidth),
// editorDefaultWidth for a nil screen.
func (s *editorScreen) columns() int {
	if s == nil {
		return editorDefaultWidth
	}
	return int(s.width.Load())
}

// setPromptWidth records the columns of the prompt the Terminal draws in
// front of the line (interactiveEditor.setPrompt).
func (s *editorScreen) setPromptWidth(width int) {
	if s != nil {
		s.promptWidth.Store(int64(width))
	}
}

// cursorRow returns the row, counted from the line's first row (where the
// prompt starts), where the Terminal put the cursor for byte offset pos of
// line.
func (s *editorScreen) cursorRow(line string, pos int) int {
	return (int(s.promptWidth.Load()) + utf8.RuneCountInString(line[:pos])) / int(s.width.Load())
}

// hideLine erases the line being edited (its cursor at byte offset pos)
// and leaves the cursor at the start of the line's first row.
func (s *editorScreen) hideLine(line string, pos int) {
	if s == nil || s.out == nil {
		return
	}
	var b strings.Builder
	if rows := s.cursorRow(line, pos); rows > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", rows)
	}
	b.WriteString("\r\x1b[J")
	_, _ = io.WriteString(s.out, b.String())
}

// restoreLine moves the cursor, at the start of a row below what was
// printed since hideLine, down by the rows the Terminal moves up from its
// cursor (for byte offset pos of line) when it next draws. The Terminal
// then clears these empty rows and draws the line there.
func (s *editorScreen) restoreLine(line string, pos int) {
	if s == nil || s.out == nil {
		return
	}
	if rows := s.cursorRow(line, pos); rows > 0 {
		_, _ = io.WriteString(s.out, strings.Repeat("\r\n", rows))
	}
}
