package clients

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/omode"
)

const interactiveControlTimeout = 2 * time.Second

type interactiveCommand struct {
	spec SessionSpec
	kind string
	next config.Args
}

type interactiveReloadState struct {
	conn               connectors.Connector
	spec               SessionSpec
	rollbackGeneration uint64
}

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
	defer cancel()

	// Must be set before runConnections starts the host key prompt.
	answers := c.routePromptAnswers()

	statusCh := make(chan int, 1)
	go func() {
		statusCh <- c.runConnections(runCtx, statsCh)
	}()

	controlErrCh := make(chan error, 1)
	go func() {
		defer answers.close()
		controlErrCh <- c.runInteractiveControl(runCtx, cancel, controlTTY, answers)
	}()

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
	if a == nil || !a.router.Prompting() {
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

// close ends the answers: a prompt still waiting gets io.EOF.
func (a *promptAnswers) close() {
	if a != nil {
		a.once.Do(func() { close(a.lines) })
	}
}

func (c *baseClient) runInteractiveControl(ctx context.Context, cancel context.CancelFunc, tty *os.File,
	answers *promptAnswers) error {
	if _, err := fmt.Fprintf(tty,
		"Interactive query control enabled. Commands: :reload <flags>, :show, :help, :quit\n"); err != nil {
		return err
	}

	reader := bufio.NewScanner(tty)
	reader.Buffer(make([]byte, 0, 1024), 1024*1024)

	go func() {
		<-ctx.Done()
		_ = tty.Close()
	}()

	for reader.Scan() {
		// The line is passed on untrimmed: parseInteractiveCommand trims it
		// only to recognise the command, so that an escaped trailing blank of
		// a :reload value (`--regex foo\ `) survives.
		line := reader.Text()
		// A line typed while the host key question is asked answers it.
		if answers.take(line) {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		currentArgs, _ := c.snapshotConnectionState()
		command, err := parseInteractiveCommand(currentArgs, line, c.clientLogger())
		if err != nil {
			if writeErr := writeControlLine(tty, "interactive query error: "+err.Error()); writeErr != nil {
				return writeErr
			}
			continue
		}

		quit, runErr := c.runInteractiveCommand(tty, command)
		if runErr != nil {
			return runErr
		}
		if quit {
			cancel()
			return nil
		}
	}

	if err := reader.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// runInteractiveCommand runs one parsed control command and writes its
// answer to the TTY. It reports quit for :quit, after which the caller
// cancels the interactive session. A failed reload is answered on the TTY and
// is no error: only a failed TTY write ends the control loop.
func (c *baseClient) runInteractiveCommand(tty io.Writer, command interactiveCommand) (quit bool, err error) {
	switch command.kind {
	case "help":
		return false, c.writeInteractiveHelp(tty)
	case "show":
		return false, c.writeInteractiveState(tty)
	case "quit":
		return true, writeControlLine(tty, "quitting interactive session")
	case "reload":
		if reloadErr := c.applyInteractiveReload(command.next, command.spec); reloadErr != nil {
			return false, writeControlLine(tty, "reload failed: "+reloadErr.Error())
		}
		return false, writeControlLine(tty, "reload applied successfully")
	default:
		return false, writeControlLine(tty, "unsupported command")
	}
}

// applyInteractiveReload switches every connection to nextSpec. Before it
// updates any connection it checks that all of them support query updates and
// every capability the read targets of nextSpec need (see
// checkReloadCapabilities), so that a reload one server cannot run is
// rejected as a whole and leaves the workload untouched. A failure after
// that rolls the already updated connections back to their previous spec.
func (c *baseClient) applyInteractiveReload(nextArgs config.Args, nextSpec SessionSpec) error {
	prevArgs, prevSpec, connections := c.snapshotMutableState()
	if len(connections) == 0 {
		return errors.New("no active connections")
	}
	if err := checkReloadCapabilities(connections, nextSpec); err != nil {
		return err
	}

	applied, generation, err := c.applyInteractiveReloadConnections(connections, nextSpec)
	if err != nil {
		return c.rollbackInteractiveReload(applied, prevArgs, prevSpec, err)
	}

	if c.profile.commit != nil {
		if commitErr := c.profile.commit(nextSpec, generation); commitErr != nil {
			return c.rollbackInteractiveReload(applied, prevArgs, prevSpec,
				fmt.Errorf("commit session state: %w", commitErr))
		}
	}

	c.storeReloadState(nextArgs, nextSpec)
	return nil
}

// checkReloadCapabilities returns an error naming the servers among
// connections that cannot run nextSpec: those not advertising query updates,
// or else those lacking the first missing capability that the read targets of
// nextSpec need (journal targets, or paths containing spaces or starting with
// base64%, see connectors.CheckSpecCapabilities). Without this up-front check
// a server lacking such a capability would fail the reload only after others
// already switched, and the rollback would re-run the previous workload on
// them, which repeats its output.
func checkReloadCapabilities(connections []connectors.Connector, nextSpec SessionSpec) error {
	var unsupported []string
	for _, conn := range connections {
		if !conn.SupportsQueryUpdates(interactiveControlTimeout) {
			unsupported = append(unsupported, conn.Server())
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("%w: %s", connectors.ErrSessionUnsupported, strings.Join(unsupported, ", "))
	}

	var missing error
	for _, conn := range connections {
		err := connectors.CheckSpecCapabilities(conn, nextSpec, interactiveControlTimeout)
		if err == nil {
			continue
		}
		if missing == nil {
			missing = err
		}
		if errors.Is(err, missing) {
			unsupported = append(unsupported, conn.Server())
		}
	}
	if missing != nil {
		return fmt.Errorf("%w: %s", missing, strings.Join(unsupported, ", "))
	}
	return nil
}

func (c *baseClient) applyInteractiveReloadConnections(connections []connectors.Connector, nextSpec SessionSpec) ([]interactiveReloadState, uint64, error) {
	var generation uint64
	baseGeneration := highestCommittedGeneration(connections)
	applied := make([]interactiveReloadState, 0, len(connections))
	for _, conn := range connections {
		prevSpec, prevGeneration, _ := conn.CommittedSession()
		if err := applyReloadSpec(conn, nextSpec, prevGeneration, baseGeneration); err != nil {
			if shouldRollbackFailedReload(err) {
				rollbackGeneration := generation
				if rollbackGeneration == 0 {
					rollbackGeneration = prevGeneration
				}
				applied = append(applied, interactiveReloadState{
					conn:               conn,
					spec:               prevSpec,
					rollbackGeneration: rollbackGeneration,
				})
			}
			return applied, 0, fmt.Errorf("%s: %w", conn.Server(), err)
		}
		applied = append(applied, interactiveReloadState{
			conn: conn,
			spec: prevSpec,
		})

		_, committedGeneration, ok := conn.CommittedSession()
		if !ok || committedGeneration == 0 {
			return applied, 0, fmt.Errorf("%s: missing committed session generation", conn.Server())
		}
		applied[len(applied)-1].rollbackGeneration = committedGeneration
		if generation == 0 {
			generation = committedGeneration
			continue
		}
		if generation != committedGeneration {
			return applied, 0, fmt.Errorf("mismatched committed generations: got %d and %d", generation, committedGeneration)
		}
	}
	return applied, generation, nil
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

// applyReloadSpec updates conn to nextSpec on top of baseGeneration, the
// highest generation of all connections, so that every connection commits
// the same next generation. A connection that reconnected meanwhile started
// its session over at generation 1 while the others kept theirs; updating
// each on top of its own generation then gave mismatched generations, and
// every later :reload was rejected and rolled back. dserver accepts any
// generation the client numbers an update with. A connection without a
// committed session starts one as before.
func applyReloadSpec(conn connectors.Connector, nextSpec SessionSpec,
	committedGeneration, baseGeneration uint64) error {

	if committedGeneration == 0 || committedGeneration == baseGeneration {
		return conn.ApplySessionSpec(nextSpec, interactiveControlTimeout)
	}
	return conn.ApplySessionSpecWithGeneration(nextSpec, baseGeneration, interactiveControlTimeout)
}

func shouldRollbackFailedReload(err error) bool {
	return errors.Is(err, connectors.ErrSessionAckTimeout) ||
		errors.Is(err, connectors.ErrUnexpectedSessionAck)
}

func (c *baseClient) rollbackInteractiveReload(applied []interactiveReloadState, prevArgs config.Args, prevSpec SessionSpec, err error) error {
	rollbackErr := c.rollbackInteractiveReloadConnections(applied)
	c.storeReloadState(prevArgs, prevSpec)
	if rollbackErr != nil {
		return errors.Join(err, rollbackErr)
	}
	return err
}

func (*baseClient) rollbackInteractiveReloadConnections(applied []interactiveReloadState) error {
	var rollbackErr error
	for i := len(applied) - 1; i >= 0; i-- {
		if err := applied[i].conn.ApplySessionSpecWithGeneration(applied[i].spec, applied[i].rollbackGeneration, interactiveControlTimeout); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: rollback session spec: %w", applied[i].conn.Server(), err))
		}
	}
	return rollbackErr
}

// writeInteractiveHelp prints the control commands and a summary of the
// :reload quoting rules implemented by splitInteractiveArgs, with an example
// for the flags of the current mode (see interactiveHelpText).
func (c *baseClient) writeInteractiveHelp(writer io.Writer) error {
	args, _ := c.snapshotConnectionState()
	return writeControlLine(writer, interactiveHelpText(args))
}

// interactiveHelpText returns the :help line for the given arguments. The
// example matches the flags parseInteractiveReloadArgs accepts in that mode,
// so that e.g. dmap or dcat do not suggest a --regex they would reject. The
// --files examples contain no spaces, as a path with a space needs servers
// advertising encoded-read-path-v1 (see doc/interactive-query.md for how to
// quote one).
func interactiveHelpText(args config.Args) string {
	const prefix = "Commands: :reload <flags>, :show, :help, :quit. Quote values as in a shell: "
	switch {
	case isInteractiveQueryMode(args):
		return prefix + "--query \"select count(status) from stats\" (double quotes keep spaces), " +
			"--files /var/log/a.log,/var/log/b.log (comma-separated)."
	case args.Mode == omode.GrepClient || args.Mode == omode.TailClient:
		return prefix + "--regex '\\d+' (single quotes keep backslashes), " +
			"--regex '' (empty value, matches every line, or none with --invert)."
	default:
		return prefix + "--files /var/log/a.log,/var/log/b.log (comma-separated)."
	}
}

// writeInteractiveState answers :show with the current session spec. It
// shows invert as well as regex, because --invert carries over between
// reloads and decides what the empty regex matches (every line, or none).
func (c *baseClient) writeInteractiveState(writer io.Writer) error {
	args, spec, connections := c.snapshotMutableState()
	ready := 0
	for _, conn := range connections {
		if conn.SupportsQueryUpdates(0) {
			ready++
		}
	}

	line := fmt.Sprintf(
		"mode=%s files=%s query=%q regex=%q invert=%t options=%q timeout=%d capable=%d/%d",
		args.Mode,
		strings.Join(spec.Files, ","),
		spec.Query,
		spec.Regex,
		spec.RegexInvert,
		spec.Options,
		spec.Timeout,
		ready,
		len(connections),
	)
	return writeControlLine(writer, line)
}

// parseInteractiveCommand parses one control line. The line is trimmed only
// to recognise the command; the flags of a :reload line are tokenized as
// typed, see parseInteractiveReload.
func parseInteractiveCommand(current config.Args, line string, logger logging.Logger) (interactiveCommand, error) {
	trimmed := strings.TrimSpace(line)

	switch {
	case trimmed == ":help":
		return interactiveCommand{kind: "help"}, nil
	case trimmed == ":show":
		return interactiveCommand{kind: "show"}, nil
	case trimmed == ":quit":
		return interactiveCommand{kind: "quit"}, nil
	case isInteractiveReloadLine(trimmed):
		return parseInteractiveReload(current, line, logger)
	default:
		return interactiveCommand{}, fmt.Errorf("unknown command %q", trimmed)
	}
}

// isInteractiveReloadLine reports whether the trimmed control line is the
// :reload command: ":reload" alone or followed by a blank (a space or a tab,
// the separators of splitInteractiveArgs). Anything else starting with
// ":reload", such as ":reloadx" or ":reload--regex=x", is an unknown command.
func isInteractiveReloadLine(trimmed string) bool {
	const command = ":reload"
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
func parseInteractiveReload(current config.Args, line string, logger logging.Logger) (interactiveCommand, error) {
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
	nextArgs, nextSpec, err := buildInteractiveSessionSpec(nextArgs, logger)
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
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bindInteractiveSharedFlags(fs, &next, current)

	var grep *string
	switch {
	case isInteractiveQueryMode(current):
		fs.StringVar(&next.QueryStr, "query", current.QueryStr, "Map reduce query")
		// Bound only so that validateInteractiveReloadArgs can reject it
		// with a reason instead of the flag package's "not defined".
		fs.Bool("invert", false, "Rejected: a query selects its own lines")
	case current.Mode == omode.GrepClient || current.Mode == omode.TailClient:
		grep = bindInteractiveGrepFlags(fs, &next, current)
	case current.Mode == omode.CatClient:
	default:
		return current, fmt.Errorf("interactive reload is unsupported for mode %s", current.Mode)
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
func buildInteractiveSessionSpec(args config.Args, logger logging.Logger) (config.Args, SessionSpec, error) {
	normalizedArgs, err := normalizeInteractiveArgs(args, logger)
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
// own table, never inverted. Other modes keep their args unchanged.
func normalizeInteractiveArgs(args config.Args, logger logging.Logger) (config.Args, error) {
	if !isInteractiveQueryMode(args) {
		return args, nil
	}

	query, err := mapr.NewQuery(args.QueryStr, logging.OrNop(logger))
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
