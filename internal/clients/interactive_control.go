package clients

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/omode"
)

const interactiveControlTimeout = 2 * time.Second

// interactiveBanner is written to the control TTY when the control loop
// starts.
const interactiveBanner = "Interactive query control enabled. Press Enter to pause the output. " +
	"Type :help for examples of changing the filter or query on the fly (Tab completes), :quit to end."

// interactivePlainBanner is the banner without the line editor (the control
// TTY is no terminal), where Tab does not complete.
const interactivePlainBanner = "Interactive query control enabled. Press Enter to pause the output. " +
	"Type :help for examples of changing the filter or query on the fly, :quit to end."

type interactiveCommand struct {
	spec SessionSpec
	kind string
	next config.Args
	// table is the optional table name of :fields.
	table string
	// value is the argument of :query (the query) and :tail (the regex,
	// empty for the one followed before), as typed.
	value string
}

type interactiveReloadState struct {
	conn               connectors.Connector
	spec               SessionSpec
	rollbackGeneration uint64
}

// startInteractiveControl runs the connections, the control loop on the
// control TTY and the background prober (interactiveProber) of an
// --interactive-query session until the connections end or the control loop
// ends (:quit, end of the TTY), and returns the session's exit status.
func (c *baseClient) startInteractiveControl(ctx context.Context, statsCh <-chan string) int {
	controlTTY, err := os.OpenFile(c.ControlTTYPath, os.O_RDWR, 0)
	if err != nil {
		c.clientLogger().Error("Unable to open interactive query control TTY", c.ControlTTYPath, err)
		return 1
	}
	defer func() {
		if closeErr := controlTTY.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			c.clientLogger().Debug("Unable to close interactive query control TTY", closeErr)
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	// The background prober is set before the goroutines that read it
	// start, and it stops with the session: it is waited for once runCtx
	// ended, which ends every wait of it at once.
	c.prober = c.newInteractiveProber()
	defer func() {
		cancel()
		c.prober.wait()
	}()

	// Must be set before runConnections starts the host key prompt.
	answers := c.routePromptAnswers()

	statusCh := make(chan int, 1)
	go func() {
		statusCh <- c.runConnections(runCtx, statsCh)
	}()
	c.prober.start(runCtx)

	controlErrCh := make(chan error, 1)
	go func() {
		defer answers.close()
		controlErrCh <- c.runInteractiveControl(runCtx, cancel, controlTTY, answers)
	}()
	return c.awaitInteractiveEnd(cancel, statusCh, controlErrCh)
}

// awaitInteractiveEnd waits until the connections (statusCh) or the control
// loop (controlErrCh) end, cancels the other one, waits for it too, and
// returns the connections' status.
func (c *baseClient) awaitInteractiveEnd(cancel context.CancelFunc, statusCh <-chan int,
	controlErrCh <-chan error) int {
	select {
	case status := <-statusCh:
		// The connections ended first: stop the control loop and wait for
		// it, so that it is done with the TTY before the deferred close. The
		// session's status decides the exit code; an error of the control
		// loop at this point (e.g. a TTY write racing with the cancellation)
		// no longer affects anything and is only logged for debugging.
		cancel()
		if controlErr := <-controlErrCh; controlErr != nil {
			c.clientLogger().Debug("Interactive query control stopped after the session ended", controlErr)
		}
		return status
	case controlErr := <-controlErrCh:
		if controlErr != nil {
			c.clientLogger().Warn("Interactive query control stopped", controlErr)
		}
		cancel()
		return <-statusCh
	}
}

// promptRouter is implemented by a host key callback whose unknown hosts
// question can be answered through the control loop.
type promptRouter interface {
	SetPromptInput(io.Reader)
	Prompting() bool
}

// promptAnswers passes lines the control loop read from the terminal on to
// the unknown hosts question of the host key callback while that is asked.
// The question reads stdin, which in an interactive session is the terminal
// the control loop reads too: without the hand-over the answer went to
// whichever of the two read it first, and "yes" was then rejected as an
// unknown control command while the question went on waiting. A nil
// *promptAnswers routes nothing.
type promptAnswers struct {
	router promptRouter
	lines  chan []byte
	rest   []byte
	once   sync.Once
}

// routePromptAnswers lets the control loop answer the host key prompt when
// the prompt would read the terminal itself, i.e. when stdin is one. With
// stdin redirected the prompt keeps reading its answers from there.
func (c *baseClient) routePromptAnswers() *promptAnswers {
	router, ok := c.hostKeyCallback.(promptRouter)
	if !ok {
		return nil
	}
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	answers := &promptAnswers{router: router, lines: make(chan []byte, 16)}
	router.SetPromptInput(answers)
	return answers
}

// take passes line on to the prompt and reports true if a question is being
// asked; otherwise the line is the control loop's.
func (a *promptAnswers) take(line string) bool {
	if !a.prompting() {
		return false
	}
	select {
	case a.lines <- []byte(line + "\n"):
	default:
		// The prompt is not reading: drop the line rather than block the
		// control loop.
	}
	return true
}

// Read hands the prompt the next line typed while it asked, and io.EOF once
// the control loop ended, which the prompt takes as a "no".
func (a *promptAnswers) Read(p []byte) (int, error) {
	if len(a.rest) == 0 {
		line, ok := <-a.lines
		if !ok {
			return 0, io.EOF
		}
		a.rest = line
	}
	n := copy(p, a.rest)
	a.rest = a.rest[n:]
	return n, nil
}

// prompting reports whether the host key question is asked, so that a line
// typed now answers it; false for nil answers.
func (a *promptAnswers) prompting() bool {
	return a != nil && a.router.Prompting()
}

// close ends the answers: a prompt still waiting gets io.EOF.
func (a *promptAnswers) close() {
	if a != nil {
		a.once.Do(func() { close(a.lines) })
	}
}

// runInteractiveControl reads control lines from the TTY until :quit, the
// end of the TTY or the cancellation of ctx, through the line editor on a
// terminal (Tab completion, history, pause while typing; see
// interactiveEditor) or plain lines otherwise. Each line answers a pending
// host key question, or is handled by handleInteractiveLine: an empty line
// pauses or resumes the output, anything else is a control command.
func (c *baseClient) runInteractiveControl(ctx context.Context, cancel context.CancelFunc, tty *os.File,
	answers *promptAnswers) error {
	// Output paused by an empty line or by typing is resumed whenever the
	// loop ends, so that writers blocked by the pause can finish and the
	// client shuts down.
	pause := c.newInteractivePause()
	defer pause.resume()
	lines, closeLines := c.openControlLines(ctx, tty, pause, answers)
	defer closeLines()
	// Written once the terminal mode is set, so that the user's first keys
	// already reach the line editor.
	if err := c.writeControlBanner(lines); err != nil {
		return err
	}

	for {
		// The line is passed on untrimmed: parseInteractiveCommand trims it
		// only to recognise the command, so that an escaped trailing blank of
		// a :reload value (`--regex foo\ `) survives.
		line, err := lines.ReadLine()
		if errors.Is(err, errControlLineTooLong) {
			if writeErr := c.rejectControlLine(lines.Writer(), err, pause); writeErr != nil {
				return writeErr
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		quit, err := c.dispatchControlLine(lines.Writer(), line, pause, answers)
		if err != nil {
			return err
		}
		if quit {
			cancel()
			return nil
		}
	}
}

// openControlLines opens the control lines of tty: the line editor (cbreak
// mode, completion, history) when tty allows it, plain lines otherwise. The
// returned close, which the control loop defers, restores the terminal mode;
// so does the cancellation of ctx, before it closes tty to end a pending
// read. While the editor runs, the statistics printed on Ctrl-C go through
// it (stats.setInterruptOutput): written straight to stdout they ran over
// the line being edited, which the Terminal did not draw again, and its
// cursor bookkeeping was off for the later edits.
func (c *baseClient) openControlLines(ctx context.Context, tty *os.File, pause *interactivePause,
	answers *promptAnswers) (controlLines, func()) {

	lines := c.newControlLines(tty, pause, answers)
	_, editing := lines.(*interactiveEditor)
	routeStats := editing && c.stats != nil
	if routeStats {
		c.stats.setInterruptOutput(lines.Writer())
	}
	// The statistics go to stdout again before the TTY is closed.
	closeLines := func() {
		if routeStats {
			c.stats.setInterruptOutput(nil)
		}
		lines.close()
	}
	go func() {
		<-ctx.Done()
		closeLines()
		_ = tty.Close()
	}()
	return lines, closeLines
}

// dispatchControlLine passes a line typed while the host key question is
// asked to that question, and any other line to handleInteractiveLine. An
// answer's keys paused the output for typing like a command's, so it is
// resumed then too.
func (c *baseClient) dispatchControlLine(w io.Writer, line string, pause *interactivePause,
	answers *promptAnswers) (bool, error) {

	if answers.take(line) {
		if pause.typing {
			pause.resume()
		}
		return false, nil
	}
	return c.handleInteractiveLine(w, line, pause)
}

// rejectControlLine reports a control line that is not run (reason) and
// resumes the output its keys paused for typing, as an entered line does.
func (c *baseClient) rejectControlLine(w io.Writer, reason error, pause *interactivePause) error {
	if pause.typing {
		pause.resume()
	}
	return writeControlLine(w, styleOf(w).failure("interactive query error: "+reason.Error()))
}

// writeControlBanner writes the banner (controlBanner) and, for a session
// that reads no files, the hint how to give it some (noFilesHint), as
// without files nothing is read and no field data can ever arrive.
func (c *baseClient) writeControlBanner(lines controlLines) error {
	w := lines.Writer()
	if err := writeControlLine(w, styleOf(w).banner(controlBanner(lines))); err != nil {
		return err
	}
	_, plain := lines.(*scannerLines)
	if hint := c.prober.noFilesHint(!plain); hint != "" {
		return writeControlLine(w, styleOf(w).hint(hint))
	}
	return nil
}

// controlBanner is the banner for lines: it mentions Tab only where the line
// editor completes.
func controlBanner(lines controlLines) string {
	if _, plain := lines.(*scannerLines); plain {
		return interactivePlainBanner
	}
	return interactiveBanner
}

// runInteractiveCommand runs one parsed control command and writes its
// answer to the TTY. It reports quit for :quit, after which the caller
// cancels the interactive session. A failed reload is answered on the TTY and
// is no error: only a failed TTY write ends the control loop.
func (c *baseClient) runInteractiveCommand(tty io.Writer, command interactiveCommand) (quit bool, err error) {
	style := styleOf(tty)
	switch command.kind {
	case "help":
		return false, c.writeInteractiveHelp(tty)
	case "show":
		return false, c.writeInteractiveState(tty)
	case "fields":
		return false, c.writeFieldsCommand(tty, command.table)
	case "probe":
		return false, c.runProbeCommand(tty)
	case "quit":
		return true, writeControlLine(tty, style.hint("quitting interactive session"))
	case "reload":
		outcome, reloadErr := c.applyInteractiveReload(command.next, command.spec)
		if reloadErr != nil {
			return false, writeControlLine(tty, style.failure("reload failed: "+reloadErr.Error()))
		}
		return false, writeControlLine(tty, reloadAppliedMessage(style, command.spec.Query)+
			outcome.message(style, c.profile.retry))
	case "query":
		return false, c.runQueryCommand(tty, command.value)
	case "showquery":
		return false, c.writeQueryState(tty)
	case "tail":
		return false, c.runTailCommand(tty, command.value)
	default:
		return false, writeControlLine(tty, style.failure("unsupported command"))
	}
}

// reloadAppliedMessage answers an applied :reload. A reload with a query
// (MapReduce mode) echoes the query it applied, highlighted with colors, so
// a mistyped word shows in another color than the one meant.
func reloadAppliedMessage(style *interactiveStyle, query string) string {
	message := style.success("reload applied successfully")
	if query == "" {
		return message
	}
	return message + "\n" + style.hint("query: ") + style.query(query)
}

// applyInteractiveReload switches every connection whose session is up to
// nextSpec, all or nothing, and the connections without an active session
// as far as they can be (see partitionReloadConnections and
// reloadRun.updateInactive). Before it updates any connection it checks
// that all connections that are up support the journal targets nextSpec
// reads, if any (see checkReloadJournalCapability), so that a reload one of
// them cannot run is rejected as a whole and leaves the workload untouched.
// A failure after that rolls the changed connections back to their
// previous spec (reloadRun.rollback). With no session up at all the reload
// is rejected: there is no generation to commit it at. A server that is
// down therefore no longer fails every reload; it starts with the reloaded
// spec should it (re)connect. Every connection is changed before the
// profile commit switches the client to the new generation (for dmap, the
// query its aggregation runs): a connection still sending results of the
// replaced workload after the commit would have them merged into the new
// one. The reload holds the reload lock throughout, so a reconnect creates its
// connection before or after it (see recreateConnection). A reload that was
// applied asks the background prober for a new round
// (interactiveProber.request).
//
// The connections are updated, and rolled back, one after the other, each
// wait capped at connectors' sessionUpdateAckMaxWait (10 s): a reload that
// fails at its last connection and whose servers all stall can take about
// 20 s per connection (update plus rollback), during which the control
// loop answers nothing. They are not changed concurrently, as the
// all-or-nothing rule needs to know which connections were changed when
// one fails, and a total budget would leave connections half updated with
// nothing to roll them back; the bound is only reached with servers that
// all stall, as one that answers does so in a round trip.
func (c *baseClient) applyInteractiveReload(nextArgs config.Args, nextSpec SessionSpec) (reloadOutcome, error) {
	reloadMu := c.reloadLock()
	reloadMu.Lock()
	defer reloadMu.Unlock()

	prevArgs, prevSpec, connections := c.snapshotMutableState()
	if len(connections) == 0 {
		return reloadOutcome{}, errors.New("no active connections")
	}
	active, inactive := partitionReloadConnections(connections)
	if len(active) == 0 {
		return reloadOutcome{}, fmt.Errorf("no server has an active session: %s", serverNames(inactive))
	}
	if err := checkReloadJournalCapability(active, nextSpec); err != nil {
		return reloadOutcome{}, err
	}

	run := &reloadRun{nextSpec: nextSpec, prevSpec: prevSpec, base: highestCommittedGeneration(active)}
	for _, conn := range active {
		if err := run.update(conn); err != nil {
			return reloadOutcome{}, c.rollbackInteractiveReload(run, prevArgs, err)
		}
	}
	outcome, err := run.updateInactive(inactive)
	if err != nil {
		return reloadOutcome{}, c.rollbackInteractiveReload(run, prevArgs, err)
	}

	if c.profile.commit != nil {
		if commitErr := c.profile.commit(nextSpec, run.generation); commitErr != nil {
			return reloadOutcome{}, c.rollbackInteractiveReload(run, prevArgs,
				fmt.Errorf("commit session state: %w", commitErr))
		}
	}

	c.storeReloadState(nextArgs, nextSpec)
	// The fields of the new files, or of the same files read anew.
	c.prober.request()
	return outcome, nil
}

// checkReloadJournalCapability returns an error naming the servers among
// connections without journal support, if nextSpec reads a journal target
// (see connectors.CheckJournalCapability). Journal support is the only
// capability a reload can lack: every protocol 5 server supports SESSION
// UPDATE and encoded read paths. Without this up-front check a server
// lacking it would fail the reload only after others already switched, and
// the rollback would re-run the previous workload on them, which repeats its
// output.
func checkReloadJournalCapability(connections []connectors.Connector, nextSpec SessionSpec) error {
	var unsupported []string
	for _, conn := range connections {
		if connectors.CheckJournalCapability(conn, nextSpec, interactiveControlTimeout) != nil {
			unsupported = append(unsupported, conn.Server())
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("%w: %s", connectors.ErrJournalUnsupported, strings.Join(unsupported, ", "))
	}
	return nil
}

// reloadRun is one :reload in progress: the connections it updated
// (applied, with what a rollback restores), the connecting ones whose start
// spec it replaced (replaced), and the generation they all committed.
type reloadRun struct {
	nextSpec SessionSpec
	prevSpec SessionSpec
	// base is the highest generation the active connections committed
	// before the reload; every connection is updated on top of it, so all
	// commit base+1.
	base uint64
	// generation is the generation the updated connections committed, 0
	// before the first update.
	generation uint64
	applied    []interactiveReloadState
	replaced   []connectors.Connector
}

// update updates conn, whose session is committed, to nextSpec on top of
// base and records it for a rollback. It fails if the update failed or conn
// committed another generation than the connections before it. A
// connection whose update was sent but not acknowledged as expected is
// rolled back too, on top of the reload's generation (or its own previous
// one if it was the first), as the server may have applied it.
func (r *reloadRun) update(conn connectors.Connector) error {
	prevSpec, prevGeneration, _ := conn.CommittedSession()
	if err := applyReloadSpec(conn, r.nextSpec, prevGeneration, r.base); err != nil {
		if shouldRollbackFailedReload(err) {
			rollbackGeneration := r.generation
			if rollbackGeneration == 0 {
				rollbackGeneration = prevGeneration
			}
			r.applied = append(r.applied, interactiveReloadState{conn: conn, spec: prevSpec,
				rollbackGeneration: rollbackGeneration})
		}
		return fmt.Errorf("%s: %w", conn.Server(), err)
	}
	_, committedGeneration, ok := conn.CommittedSession()
	r.applied = append(r.applied, interactiveReloadState{conn: conn, spec: prevSpec,
		rollbackGeneration: committedGeneration})
	if !ok || committedGeneration == 0 {
		return fmt.Errorf("%s: missing committed session generation", conn.Server())
	}
	if r.generation == 0 {
		r.generation = committedGeneration
		return nil
	}
	if r.generation != committedGeneration {
		return fmt.Errorf("mismatched committed generations: got %d and %d", r.generation, committedGeneration)
	}
	return nil
}

// highestCommittedGeneration returns the highest session generation any of
// connections committed, 0 if none committed one.
func highestCommittedGeneration(connections []connectors.Connector) uint64 {
	var highest uint64
	for _, conn := range connections {
		if _, generation, ok := conn.CommittedSession(); ok && generation > highest {
			highest = generation
		}
	}
	return highest
}

// applyReloadSpec updates conn, whose session is committed at
// committedGeneration, to nextSpec on top of baseGeneration, the highest
// generation of the active connections, so that every connection commits
// the same next generation. A connection that reconnected meanwhile started
// its session over at generation 1 while the others kept theirs; updating
// each on top of its own generation then gave mismatched generations, and
// every later :reload was rejected and rolled back. dserver accepts any
// generation the client numbers an update with. A reload updates committed
// sessions only: a connection still connecting gets the reloaded spec for
// its SESSION START instead (see reloadRun.updateInactive).
func applyReloadSpec(conn connectors.Connector, nextSpec SessionSpec,
	committedGeneration, baseGeneration uint64) error {

	if committedGeneration == baseGeneration {
		return conn.ApplySessionSpec(nextSpec, interactiveControlTimeout)
	}
	return conn.ApplySessionSpecWithGeneration(nextSpec, baseGeneration, interactiveControlTimeout)
}

func shouldRollbackFailedReload(err error) bool {
	return errors.Is(err, connectors.ErrSessionAckTimeout) ||
		errors.Is(err, connectors.ErrUnexpectedSessionAck)
}

// rollbackInteractiveReload rolls back the connections run changed (see
// reloadRun.rollback), restores the client state of before the reload and
// returns err together with any rollback failure.
func (c *baseClient) rollbackInteractiveReload(run *reloadRun, prevArgs config.Args, err error) error {
	rollbackErr := run.rollback()
	c.storeReloadState(prevArgs, run.prevSpec)
	if rollbackErr != nil {
		return errors.Join(err, rollbackErr)
	}
	return err
}

// rollback gives the connecting connections back the previous spec as
// their start spec (see restoreStartSpecs) and updates the updated
// connections, in reverse order, back to their previous spec.
func (r *reloadRun) rollback() error {
	rollbackErr := restoreStartSpecs(r.replaced, r.prevSpec)
	for i := len(r.applied) - 1; i >= 0; i-- {
		state := r.applied[i]
		if err := state.conn.ApplySessionSpecWithGeneration(state.spec, state.rollbackGeneration,
			interactiveControlTimeout); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: rollback session spec: %w", state.conn.Server(), err))
		}
	}
	return rollbackErr
}

// writeInteractiveHelp prints the control commands and a summary of the
// :reload quoting rules implemented by splitInteractiveArgs, with an example
// for the flags of the current mode (see interactiveHelpText).
func (c *baseClient) writeInteractiveHelp(writer io.Writer) error {
	args, _ := c.snapshotConnectionState()
	return writeControlLine(writer, styleOf(writer).help(interactiveHelpText(args)))
}

// interactiveHelpText returns the :help text for the given arguments: the
// commands, then examples of changing the workload on the fly with :reload.
// The examples match the flags parseInteractiveReloadArgs accepts in that
// mode, so that e.g. dmap or dcat do not suggest a --regex they would reject,
// and the query examples are valid queries. The --files examples contain no
// spaces (see doc/interactive-query.md for how to quote such a path). The
// commands line names :query and :tail only in the modes that have them,
// and the examples show switching between following lines and a query in
// dtail (helpSwitchExamples).
func interactiveHelpText(args config.Args) string {
	head := helpCommandsLine(args) + " An empty line pauses or resumes the output.\n" +
		"On a terminal typing pauses the output until the line is entered, Tab completes commands, " +
		"flags, after a filter, query or files flag its current value, inside a query the query language " +
		"and probed fields, inside --files remote paths; " +
		"Up/Down recall earlier lines; Alt-E (or Alt-V) edits the line in $VISUAL/$EDITOR; " +
		"--edit-mode vi (or Client.EditMode) adds vi normal mode keys after Esc.\n" +
		":fields lists the tables, fields and sample values the background probe of the servers found " +
		"(Tab completes them in a query); :probe probes now.\n"
	const tail = "Flags not given keep their value; :show prints the current ones. " +
		"Quote :reload values as in a shell; the query of :query and the regex of :tail are " +
		"the rest of the line, unquoted (see doc/interactive-query.md)."
	files := [2]string{":reload --files /var/log/a.log,/var/log/b.log", "other files (comma-separated)"}
	switch {
	case isInteractiveQueryMode(args):
		return head + "Change the MapReduce query on the fly, e.g.:\n" + helpExamples(append(
			helpSwitchExamples(args),
			[2]string{":reload --query 'from STATS select count($line) group by $hostname interval 10'", ""},
			files)...) + tail
	case args.Mode == omode.GrepClient || args.Mode == omode.TailClient:
		return head + "Change the grep filter on the fly, e.g.:\n" + helpExamples(append(
			helpSwitchExamples(args),
			[2]string{":reload --grep ERROR", "only lines matching ERROR"},
			[2]string{":reload --grep 'WARN|ERROR' --before 2 --after 2", "a regex, with context lines"},
			[2]string{`:reload --regex '\d+ ms'`, "single quotes keep backslashes"},
			[2]string{":reload --grep DEBUG --invert", "every line but DEBUG (--invert=false undoes it)"},
			[2]string{":reload --regex ''", "every line again (none with --invert)"},
			files)...) + tail
	default:
		return head + "Change what is read on the fly, e.g.:\n" + helpExamples(files) + tail
	}
}

// helpCommandsLine returns the first line of :help, naming the commands of
// the mode of args.
func helpCommandsLine(args config.Args) string {
	commands := []string{":reload <flags>"}
	if hasQueryCommand(args) {
		commands = append(commands, ":query [query]")
	}
	if hasTailCommand(args) {
		commands = append(commands, ":tail [regex]")
	}
	commands = append(commands, ":show", ":fields [table]", ":probe", ":help", ":quit")
	return "Commands: " + strings.Join(commands, ", ") + "."
}

// helpSwitchExamples returns the :query and :tail examples of the mode of
// args: in dtail following lines the switch to a query, in dtail running a
// query other queries and the way back to following lines, in dmap other
// queries; none in dgrep and dcat.
func helpSwitchExamples(args config.Args) [][2]string {
	// Short enough for the explanation column of helpExamples.
	const query = ":query from STATS select count($line)"
	switch {
	case !hasQueryCommand(args):
		return nil
	case !isInteractiveQueryMode(args):
		return [][2]string{{query, "switch to a MapReduce query (Tab completes it)"}}
	}
	examples := [][2]string{
		{query, "another query (:query alone shows the current one)"},
		{":query from STATS select $hostname,max($goroutines) group by $hostname order by max($goroutines)", ""},
	}
	if hasTailCommand(args) {
		examples = append(examples,
			[2]string{":tail", "follow lines again, with the regex followed before"},
			[2]string{":tail ERROR", "follow lines matching ERROR"})
	}
	return examples
}

// helpExamples formats :help example lines, each a command and an optional
// explanation, the explanations aligned in one column.
func helpExamples(examples ...[2]string) string {
	var sb strings.Builder
	for _, example := range examples {
		if example[1] == "" {
			fmt.Fprintf(&sb, "  %s\n", example[0])
			continue
		}
		fmt.Fprintf(&sb, "  %-50s %s\n", example[0], example[1])
	}
	return sb.String()
}

// writeInteractiveState answers :show with the workload (workload=: query
// while a MapReduce query runs, else the mode), the current session spec, the
// number of servers (servers=) and the number of them whose session is up
// (sessions=, see partitionReloadConnections), which a :reload updates; the
// others start with the reloaded spec if they still connect. It shows
// invert as well as regex, because --invert carries over between reloads and
// decides what the empty regex matches (every line, or none). With colors
// the query is highlighted (interactiveStyle.quotedQuery), which leaves the
// text as it is.
func (c *baseClient) writeInteractiveState(writer io.Writer) error {
	args, spec, connections := c.snapshotMutableState()
	active, _ := partitionReloadConnections(connections)
	style := styleOf(writer)
	pairs := []keyValue{
		{key: "mode", value: args.Mode.String()},
		{key: "workload", value: workloadName(args)},
		{key: "files", value: strings.Join(spec.Files, ",")},
		{key: "query", value: style.quotedQuery(spec.Query), painted: style != nil},
		{key: "regex", value: strconv.Quote(spec.Regex)},
		{key: "invert", value: strconv.FormatBool(spec.RegexInvert)},
		{key: "options", value: strconv.Quote(spec.Options)},
		{key: "timeout", value: strconv.Itoa(spec.Timeout)},
		{key: "servers", value: strconv.Itoa(len(connections))},
		{key: "sessions", value: strconv.Itoa(len(active))},
	}
	return writeControlLine(writer, style.keyValues(pairs))
}

// parseInteractiveCommand parses one control line. The line is trimmed only
// to recognise the command; the flags of a :reload line are tokenized as
// typed, see parseInteractiveReload, and the argument of :query and :tail is
// the rest of the line (parseQueryCommand, parseTailCommand).
func parseInteractiveCommand(current config.Args, line string) (interactiveCommand, error) {
	trimmed := strings.TrimSpace(line)

	switch {
	case trimmed == ":help":
		return interactiveCommand{kind: "help"}, nil
	case trimmed == ":show":
		return interactiveCommand{kind: "show"}, nil
	case trimmed == ":quit":
		return interactiveCommand{kind: "quit"}, nil
	case trimmed == ":probe":
		return interactiveCommand{kind: "probe"}, nil
	case isCommandLine(trimmed, ":probe"):
		return interactiveCommand{}, errors.New(":probe takes no arguments")
	case isCommandLine(trimmed, ":fields"):
		return parseFieldsCommand(trimmed)
	case isCommandLine(trimmed, queryCommand):
		return parseQueryCommand(current, line)
	case isCommandLine(trimmed, tailCommand):
		return parseTailCommand(current, line)
	case isInteractiveReloadLine(trimmed):
		return parseInteractiveReload(current, line)
	default:
		return interactiveCommand{}, fmt.Errorf("unknown command %q", trimmed)
	}
}

// isInteractiveReloadLine reports whether the trimmed control line is the
// :reload command (see isCommandLine). Anything else starting with
// ":reload", such as ":reloadx" or ":reload--regex=x", is an unknown command.
func isInteractiveReloadLine(trimmed string) bool {
	return isCommandLine(trimmed, ":reload")
}

// isCommandLine reports whether the trimmed control line is command alone
// or followed by a blank (a space or a tab, the separators of
// splitInteractiveArgs) and its arguments.
func isCommandLine(trimmed, command string) bool {
	rest, found := strings.CutPrefix(trimmed, command)
	return found && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// parseInteractiveReload parses a :reload line into the next arguments and
// session spec. Only leading blanks and a trailing line ending are removed
// before the flags are tokenized: trailing blanks are separators to the
// tokenizer anyway, while an escaped one (`--regex foo\ `) is part of the
// value, as in a POSIX shell, and must not be trimmed away first.
//
// A line that is not valid UTF-8 is rejected: the SESSION UPDATE payload is
// the JSON encoding of the spec, and encoding/json would silently replace
// each invalid byte with U+FFFD, so e.g. a file path would change.
func parseInteractiveReload(current config.Args, line string) (interactiveCommand, error) {
	if !utf8.ValidString(line) {
		return interactiveCommand{}, errors.New("reload line is not valid UTF-8")
	}
	remainder := strings.TrimPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), ":reload")
	remainder = strings.TrimRight(remainder, "\r\n")
	if strings.TrimSpace(remainder) == "" {
		return interactiveCommand{}, errors.New("reload requires flags to change")
	}
	tokens, err := splitInteractiveArgs(remainder)
	if err != nil {
		return interactiveCommand{}, err
	}
	nextArgs, err := parseInteractiveReloadArgs(current, tokens)
	if err != nil {
		return interactiveCommand{}, err
	}
	nextArgs, nextSpec, err := buildInteractiveSessionSpec(nextArgs)
	if err != nil {
		return interactiveCommand{}, err
	}
	return interactiveCommand{
		kind: "reload",
		next: nextArgs,
		spec: nextSpec,
	}, nil
}

// parseInteractiveReloadArgs applies the flags of a `:reload` line to a copy
// of the current arguments. Which flags exist depends on the mode (see
// doc/interactive-query.md); on any error the current arguments are returned
// unchanged together with the error.
func parseInteractiveReloadArgs(current config.Args, tokens []string) (config.Args, error) {
	next := current
	fs, grep, err := newInteractiveReloadFlagSet(current, &next)
	if err != nil {
		return current, err
	}

	if err := fs.Parse(tokens); err != nil {
		return current, err
	}
	if len(fs.Args()) > 0 {
		return current, fmt.Errorf("unexpected arguments: %s", quoteInteractiveArgs(fs.Args()))
	}
	// --grep is an alias of --regex with the semantics of the dgrep and dtail
	// command lines (cmd/dgrep, cmd/dtail): a non-empty --grep wins over
	// --regex whatever their order. An empty --grep, which the command lines
	// ignore, is rejected by validateInteractiveReloadArgs, so `--regex ''`
	// is the way to clear the regex.
	if grep != nil && *grep != "" {
		next.RegexStr = *grep
	}
	if err := validateInteractiveReloadArgs(fs, next); err != nil {
		return current, err
	}
	return next, nil
}

// newInteractiveReloadFlagSet returns the :reload flag set of the mode of
// current, its flags bound to next (initialised from current). It is the one
// definition of which flags a mode accepts, used by the parser and by the
// Tab completion (interactiveReloadFlags). grep is the --grep alias value of
// the grep-driven modes, nil otherwise.
func newInteractiveReloadFlagSet(current config.Args, next *config.Args) (fs *flag.FlagSet, grep *string, err error) {
	fs = flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bindInteractiveSharedFlags(fs, next, current)

	switch {
	case isInteractiveQueryMode(current):
		fs.StringVar(&next.QueryStr, "query", current.QueryStr, "Map reduce query")
		// Bound only so that validateInteractiveReloadArgs can reject it
		// with a reason instead of the flag package's "not defined".
		fs.Bool("invert", false, "Rejected: a query selects its own lines")
	case current.Mode == omode.GrepClient || current.Mode == omode.TailClient:
		grep = bindInteractiveGrepFlags(fs, next, current)
	case current.Mode == omode.CatClient:
	default:
		return nil, nil, fmt.Errorf("interactive reload is unsupported for mode %s", current.Mode)
	}
	return fs, grep, nil
}

// bindInteractiveSharedFlags binds the :reload flags every mode accepts.
func bindInteractiveSharedFlags(fs *flag.FlagSet, next *config.Args, current config.Args) {
	fs.StringVar(&next.What, "files", current.What, "File(s) to read")
	fs.BoolVar(&next.Plain, "plain", current.Plain, "Plain output mode")
	fs.BoolVar(&next.Quiet, "quiet", current.Quiet, "Quiet output mode")
	fs.IntVar(&next.Timeout, "timeout", current.Timeout, "Max time dtail server will collect data until disconnection")
}

// bindInteractiveGrepFlags binds the :reload flags of the grep-driven modes
// (dgrep and dtail without --query). It returns the --grep alias value, which
// the caller applies after parsing, see parseInteractiveReloadArgs.
func bindInteractiveGrepFlags(fs *flag.FlagSet, next *config.Args, current config.Args) *string {
	fs.StringVar(&next.RegexStr, "regex", current.RegexStr, "Regular expression")
	grep := fs.String("grep", "", "Alias for -regex")
	fs.BoolVar(&next.RegexInvert, "invert", current.RegexInvert, "Invert regex")
	fs.IntVar(&next.BeforeContext, "before", current.BeforeContext, "Leading context lines")
	fs.IntVar(&next.AfterContext, "after", current.AfterContext, "Trailing context lines")
	fs.IntVar(&next.MaxCount, "max", current.MaxCount, "Maximum number of matches")
	return grep
}

// validateInteractiveReloadArgs rejects explicitly given values that would
// leave the session without a workload: a --files value without any
// non-blank file (an empty or blank value, or only commas) would cancel the
// running work and read nothing, and a blank --query would silently turn a
// query session into a plain one (dtail) or an unsupported one (dmap), after
// which --query could not even be reloaded anymore. An empty --grep is
// rejected as well: the command lines ignore it, so a reload would restart
// the unchanged workload (in dgrep reading and printing the file again)
// although the user most likely meant to clear the regex. In a query session
// --invert (unless --invert=false) is rejected with errReloadQueryInvert, as
// the query's table filter is never inverted (applyQueryRegex). Values not given
// on the line keep the current ones and are not checked, so e.g. a
// serverless stdin session with no files can still reload other flags.
func validateInteractiveReloadArgs(fs *flag.FlagSet, next config.Args) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		switch {
		case err != nil:
		case f.Name == "files" && !hasNonBlankFile(next.What):
			err = fmt.Errorf("--files needs at least one non-blank file, got %q", next.What)
		case f.Name == "query" && strings.TrimSpace(next.QueryStr) == "":
			err = fmt.Errorf("--query must not be blank, got %q", next.QueryStr)
		case f.Name == "grep" && f.Value.String() == "":
			err = errors.New("--grep '' is ignored; use --regex '' to clear the regex")
		case f.Name == "invert" && isInteractiveQueryMode(next) && f.Value.String() == "true":
			err = errReloadQueryInvert
		}
	})
	return err
}

// hasNonBlankFile reports whether a comma-separated --files value names at
// least one file, splitting it like the session spec does.
func hasNonBlankFile(what string) bool {
	for _, file := range strings.Split(what, ",") {
		if strings.TrimSpace(file) != "" {
			return true
		}
	}
	return false
}

// quoteInteractiveArgs formats leftover arguments with %q each, so that empty
// or blank arguments stay visible in the error message.
func quoteInteractiveArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = fmt.Sprintf("%q", arg)
	}
	return strings.Join(quoted, " ")
}

// buildInteractiveSessionSpec normalizes the reloaded args
// (normalizeInteractiveArgs) and returns them together with the session spec
// built from them, after checking that the spec yields valid commands. The
// caller stores both, so the client's args (read by reconnects and :show)
// carry the same line filter as the spec sent to the servers.
func buildInteractiveSessionSpec(args config.Args) (config.Args, SessionSpec, error) {
	normalizedArgs, err := normalizeInteractiveArgs(args)
	if err != nil {
		return args, SessionSpec{}, err
	}

	spec := NewSessionSpec(normalizedArgs)
	if _, commandErr := spec.Commands(); commandErr != nil {
		return args, SessionSpec{}, commandErr
	}
	return normalizedArgs, spec, nil
}

// normalizeInteractiveArgs derives the line filter of an interactive query
// from the query (applyQueryRegex), so a reloaded query reads the lines of its
// own table, never inverted. Other modes keep their args unchanged. The query
// is parsed without a logger (parsing does not log, the logger of a Query
// serves only its evaluation and this one is discarded), so a :reload is
// parsed while the output is paused without blocking on the paused client
// logger (handleInteractiveLine).
func normalizeInteractiveArgs(args config.Args) (config.Args, error) {
	if !isInteractiveQueryMode(args) {
		return args, nil
	}

	query, err := mapr.NewQuery(args.QueryStr, nil)
	if err != nil {
		return args, err
	}
	applyQueryRegex(&args, query)
	return args, nil
}

func isInteractiveQueryMode(args config.Args) bool {
	return strings.TrimSpace(args.QueryStr) != "" &&
		(args.Mode == omode.MapClient || args.Mode == omode.TailClient)
}

func writeControlLine(writer io.Writer, message string) error {
	_, err := fmt.Fprintf(writer, "%s\n", message)
	return err
}
