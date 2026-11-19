package clients

import (
	"io"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
)

// Messages the control loop writes to the control TTY when an empty line
// pauses or resumes the output, and the hint the line editor shows when
// typing paused it.
const (
	interactivePausedMessage  = "output paused, press Enter to resume or type a command"
	interactiveResumedMessage = "output resumed"
	interactiveTypingHint     = "(output paused while you type: Tab completes, :help shows commands and examples, " +
		"Enter runs the line)"
)

// outputGate holds back writes to the serverless direct output while the
// interactive output is paused. That output bypasses the client logger
// (handlers.Dependencies.ServerlessOutput), so pausing the logger alone would
// let serverless payload go on scrolling. A nil *outputGate never blocks.
type outputGate struct {
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
}

func newOutputGate() *outputGate {
	g := &outputGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Pause makes later writes through the gate wait until Resume. A write that
// already passed the gate still completes.
func (g *outputGate) Pause() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused = true
}

// Resume releases every write waiting at the gate.
func (g *outputGate) Resume() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused = false
	g.cond.Broadcast()
}

// wait blocks while the gate is paused.
func (g *outputGate) wait() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.paused {
		g.cond.Wait()
	}
}

// writer wraps w so that every write first waits for the gate to be open.
// Blocking the writer applies backpressure to the in-process server, like a
// paused pager does to a remote one.
func (g *outputGate) writer(w io.Writer) io.Writer {
	if g == nil {
		return w
	}
	return gatedWriter{gate: g, w: w}
}

type gatedWriter struct {
	gate *outputGate
	w    io.Writer
}

func (w gatedWriter) Write(p []byte) (int, error) {
	w.gate.wait()
	return w.w.Write(p)
}

// interactivePause is the pause state of the interactive control loop: an
// empty line pauses the client's terminal output, so that a command can be
// typed without output scrolling over it, and the next empty line resumes
// it. With the line editor (interactiveEditor) the first key of a command
// pauses it too (pauseTyping), and the command's end resumes it. Pausing
// blocks the writers (client logger and serverless direct output), which
// applies backpressure to the servers instead of buffering output without
// bound. It is used by the control loop goroutine only (the editor's key
// callback runs inside that goroutine's ReadLine).
type interactivePause struct {
	logger logging.Logger
	gate   *outputGate
	paused bool
	// typing is true while the output is paused only because a command is
	// being typed: the line's end resumes it, without a message.
	typing bool
	// prober is told about every pause and resume, as it holds back its
	// periodic rounds while the output is paused; nil without one.
	prober *interactiveProber
}

func (c *baseClient) newInteractivePause() *interactivePause {
	var gate *outputGate
	if c.runtime != nil {
		gate = c.runtime.outputGate
	}
	return &interactivePause{logger: c.clientLogger(), gate: gate, prober: c.prober}
}

// pause stops the output unless it is paused already. The logger's pause is
// depth counted, so it combines with the pauses of the host key prompt and
// the interrupt statistics.
func (p *interactivePause) pause() {
	if p.paused {
		return
	}
	p.paused = true
	p.typing = false
	p.prober.setOutputPaused(true)
	clientlog.Pause(p.logger)
	p.gate.Pause()
}

// pauseTyping pauses the output while a command is typed, unless it is
// paused already (an explicit pause then stays explicit). It reports whether
// this call paused it, so the editor shows its hint once per typed line.
func (p *interactivePause) pauseTyping() bool {
	if p.paused {
		return false
	}
	p.pause()
	p.typing = true
	return true
}

// resume restarts the output if this loop paused it, and reports whether it
// did.
func (p *interactivePause) resume() bool {
	if !p.paused {
		return false
	}
	p.paused = false
	p.typing = false
	p.gate.Resume()
	clientlog.Resume(p.logger)
	p.prober.setOutputPaused(false)
	return true
}

// runOutput runs fn with the output running: a paused output is released
// for the time of fn and paused again after it, keeping the pause state
// (explicit or for typing) as it was, and the prober is not told (it is no
// resume for its periodic rounds). Tab uses it to wait for a path listing,
// whose answer arrives only while the output runs (waitForListing).
func (p *interactivePause) runOutput(fn func()) {
	if !p.paused {
		fn()
		return
	}
	p.gate.Resume()
	clientlog.Resume(p.logger)
	defer func() {
		clientlog.Pause(p.logger)
		p.gate.Pause()
	}()
	fn()
}

// handleInteractiveLine handles one control line other than a host key
// answer. An empty (or blank) line toggles the pause: it pauses the output
// and prints the help, or resumes it. Commands run while explicitly paused
// leave the output paused, also rejected ones, except a :reload, :probe,
// :query or :tail that parsed, which resumes it before it runs
// (resumesBeforeRun). Output
// paused only for typing (pauseTyping) is resumed silently once the line
// was handled, except after :help and :fields, which
// keep it paused until the next empty line (keepPausedAfter), so that their
// answer does not scroll away. :fields TABLE first lets the output run
// while it waits briefly for a table probe of TABLE (awaitTableProbe).
// :quit ends the loop, whose caller resumes the output for the shutdown.
func (c *baseClient) handleInteractiveLine(tty io.Writer, line string,
	pause *interactivePause) (bool, error) {

	typing := pause.paused && pause.typing
	if strings.TrimSpace(line) == "" {
		return false, c.togglePause(tty, pause, typing)
	}
	// A typed line's answer is printed while still paused, then output goes
	// on, except after :help and :fields (see keepPausedAfter).
	resumeAfter := typing
	defer func() {
		if resumeAfter {
			pause.resume()
		}
	}()
	// Parse while still paused: parsing never writes to the output (a
	// reload's query is parsed without a logger), so a rejected command is
	// answered with the output paused and its error does not scroll away.
	currentArgs, _ := c.snapshotConnectionState()
	command, err := parseInteractiveCommand(currentArgs, line)
	if err != nil {
		return false, writeControlLine(tty, styleOf(tty).failure("interactive query error: "+err.Error()))
	}
	if resumesBeforeRun(command.kind) && pause.resume() && !typing {
		if writeErr := writeControlLine(tty, styleOf(tty).paused(interactiveResumedMessage)); writeErr != nil {
			return false, writeErr
		}
	}
	if command.kind == "fields" && command.table != "" {
		// :fields TABLE shows what a table probe of TABLE found: one is
		// asked for and waited for briefly with the output running, as its
		// answer is only read then (awaitTableProbe).
		c.awaitTableProbe(pause, command.table)
	}
	if keepsPause(command.kind) && (typing || !pause.paused) {
		resumeAfter = false
		return false, c.keepPausedAfter(tty, pause, command)
	}
	return c.runInteractiveCommand(tty, command)
}

// togglePause handles an empty line: it resumes paused output (silently
// after a pause for typing) or pauses the output and prints the help.
func (c *baseClient) togglePause(tty io.Writer, pause *interactivePause, typing bool) error {
	if pause.resume() {
		if typing {
			return nil
		}
		return writeControlLine(tty, styleOf(tty).paused(interactiveResumedMessage))
	}
	pause.pause()
	if err := writeControlLine(tty, styleOf(tty).paused(interactivePausedMessage)); err != nil {
		return err
	}
	return c.writeInteractiveHelp(tty)
}

// resumesBeforeRun reports whether a parsed command of kind needs the output
// running: the servers' acknowledgement of a :reload (and of the switches
// :query and :tail, which update the session the same way) and their answer to a
// :probe are read by the same connection handlers that write the output, so
// with the output blocked a reload would time out and a probe would get no
// answer. It is asked only once the line parsed, so a rejected :reload or
// :probe keeps the pause. A reload that parsed but then fails to apply (e.g.
// a server lacking journal-v1, no server with an active session) has resumed
// the output already.
func resumesBeforeRun(kind string) bool {
	return kind == "reload" || kind == "probe" || kind == "query" || kind == "tail"
}

// keepsPause reports whether a command of kind answers with the output
// paused (keepPausedAfter): the ones that print a listing to read.
func keepsPause(kind string) bool {
	return kind == "help" || kind == "fields"
}

// keepPausedAfter answers command (:help or :fields) with the output
// paused: the answer would scroll away at once if the output went on. A
// pause for typing becomes an explicit one, as if Enter had paused it, and
// output that was not paused (no line editor) is paused; the next empty line
// resumes it.
func (c *baseClient) keepPausedAfter(tty io.Writer, pause *interactivePause, command interactiveCommand) error {
	if pause.typing {
		pause.typing = false
	} else {
		pause.pause()
	}
	if _, err := c.runInteractiveCommand(tty, command); err != nil {
		return err
	}
	return writeControlLine(tty, styleOf(tty).paused(interactivePausedMessage))
}
