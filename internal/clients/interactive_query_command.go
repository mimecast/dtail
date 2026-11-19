package clients

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// The :query and :tail commands switch an interactive dtail between a
// MapReduce query and following lines on its running session
// (switchToQuery, switchToTail). Their argument is the rest of the line as
// typed: no shell quoting, as the query language has double quotes of its
// own; only the blanks around it are dropped (a regex ending in a blank is
// written with a class, e.g. `foo[ ]`). dmap, which always runs a query,
// has :query to replace it; dgrep and dcat have neither.
const (
	queryCommand = ":query"
	tailCommand  = ":tail"
)

var (
	// errQueryCommandUnsupported rejects :query outside dtail and dmap.
	errQueryCommandUnsupported = errors.New(":query is only available in dtail and dmap " +
		"(dgrep and dcat read lines only)")
	// errTailCommandUnsupported rejects :tail outside dtail.
	errTailCommandUnsupported = errors.New(":tail is only available in dtail")
	// errNoQuerySession rejects a :query on a client that can neither
	// switch its session to a query (interactive dtail) nor runs one it
	// could replace (dmap); the parser lets only those modes through, so
	// this guards against a client built otherwise.
	errNoQuerySession = errors.New("this client can neither switch its session to a query nor replace one")
)

// hasQueryCommand reports whether the mode of args has :query: dtail (to
// switch to a query or replace it) and dmap (to replace its query). Only
// interactive clients run the control loop, so the mode decides.
func hasQueryCommand(args config.Args) bool {
	return args.Mode == omode.TailClient || args.Mode == omode.MapClient
}

// hasTailCommand reports whether the mode of args has :tail: dtail only.
func hasTailCommand(args config.Args) bool {
	return args.Mode == omode.TailClient
}

// commandArgument returns the argument of a :query or :tail line: the rest
// of line after command, without the blanks around it. The line must be
// valid UTF-8, as the argument goes into the JSON of a SESSION UPDATE (see
// parseInteractiveReload).
func commandArgument(line, command string) (string, error) {
	if !utf8.ValidString(line) {
		return "", fmt.Errorf("%s line is not valid UTF-8", command)
	}
	rest := strings.TrimPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), command)
	return strings.TrimSpace(rest), nil
}

// parseQueryCommand parses a :query line: with a query, the switch to (or
// the replacement by) that query; alone, showing the query that runs. The
// query must parse and yield valid read commands (checkQuerySpec) while
// the line is parsed, before resumesBeforeRun resumes paused output, so a
// rejected query is answered with the output still paused.
func parseQueryCommand(current config.Args, line string) (interactiveCommand, error) {
	if !hasQueryCommand(current) {
		return interactiveCommand{}, errQueryCommandUnsupported
	}
	query, err := commandArgument(line, queryCommand)
	if err != nil {
		return interactiveCommand{}, err
	}
	if query == "" {
		return interactiveCommand{kind: "showquery"}, nil
	}
	if err := checkQuerySpec(current, query); err != nil {
		return interactiveCommand{}, err
	}
	return interactiveCommand{kind: "query", value: query}, nil
}

// checkQuerySpec builds the session spec a switch to query would send
// (buildInteractiveSessionSpec: the query parsed without a logger, its
// table's regex, the read commands), as switchToQuery and applyQueryCommand
// do once the command runs, without the tail-only filter they drop.
func checkQuerySpec(current config.Args, query string) error {
	next := current
	next.QueryStr = query
	clearTailFilter(&next)
	if _, _, err := buildInteractiveSessionSpec(next); err != nil {
		return fmt.Errorf("invalid query: %w", err)
	}
	return nil
}

// parseTailCommand parses a :tail line: the switch to following lines, with
// the regex followed before or the one given. A given regex must yield
// valid read commands (the session spec is built as switchToTail builds
// it) while the line is parsed, so a rejected one keeps paused output
// paused; the regex followed before was valid when it ran.
func parseTailCommand(current config.Args, line string) (interactiveCommand, error) {
	if !hasTailCommand(current) {
		return interactiveCommand{}, errTailCommandUnsupported
	}
	pattern, err := commandArgument(line, tailCommand)
	if err != nil {
		return interactiveCommand{}, err
	}
	if pattern != "" {
		next := current
		next.QueryStr = ""
		next.RegexStr = pattern
		next.RegexInvert = false
		if _, _, err := buildInteractiveSessionSpec(next); err != nil {
			return interactiveCommand{}, fmt.Errorf("invalid regex: %w", err)
		}
	}
	return interactiveCommand{kind: "tail", value: pattern}, nil
}

// runQueryCommand applies a parsed :query and answers it on the TTY: the
// query applied (echoed, highlighted) or why it failed, in which case the
// running workload is left as it was. The caller resumed paused output
// first (resumesBeforeRun).
func (c *baseClient) runQueryCommand(tty io.Writer, query string) error {
	style := styleOf(tty)
	outcome, err := c.applyQueryCommand(query)
	if err != nil {
		return writeControlLine(tty, style.failure("query failed: "+errorLines(err)))
	}
	return writeControlLine(tty, style.success("query applied")+"\n"+style.hint("query: ")+
		style.query(query)+outcome.message(style, c.profile.retry))
}

// applyQueryCommand switches the session of interactive dtail to query
// (switchToQuery), or replaces the query of dmap like :reload --query.
func (c *baseClient) applyQueryCommand(query string) (reloadOutcome, error) {
	if c.canSwitchWorkload() {
		return c.switchToQuery(query)
	}
	current, _ := c.snapshotConnectionState()
	if !isInteractiveQueryMode(current) {
		return reloadOutcome{}, errNoQuerySession
	}
	current.QueryStr = query
	next, spec, err := buildInteractiveSessionSpec(current)
	if err != nil {
		return reloadOutcome{}, fmt.Errorf("invalid query: %w", err)
	}
	return c.applyInteractiveReload(next, spec)
}

// runTailCommand applies a parsed :tail (switchToTail) and answers it on
// the TTY with the regex the session now follows, or why it failed, in
// which case the running workload is left as it was.
func (c *baseClient) runTailCommand(tty io.Writer, pattern string) error {
	style := styleOf(tty)
	outcome, err := c.switchToTail(pattern)
	if err != nil {
		return writeControlLine(tty, style.failure("tail failed: "+errorLines(err)))
	}
	args, _ := c.snapshotConnectionState()
	message := style.success("following lines") + "\n" + style.hint("regex: ") + strconv.Quote(args.RegexStr)
	if args.RegexInvert {
		message += style.hint(" (inverted)")
	}
	return writeControlLine(tty, message+outcome.message(style, c.profile.retry))
}

// writeQueryState answers :query alone with the query that runs,
// highlighted, or that none does while the session follows lines.
func (c *baseClient) writeQueryState(tty io.Writer) error {
	style := styleOf(tty)
	args, _ := c.snapshotConnectionState()
	if !isInteractiveQueryMode(args) {
		return writeControlLine(tty, style.hint("no query runs: the session follows lines; "+
			":query <query> starts one"))
	}
	return writeControlLine(tty, style.hint("query: ")+style.query(args.QueryStr))
}

// workloadName names what the session runs for :show: "query" while a
// MapReduce query runs, otherwise the mode (tail, grep, cat).
func workloadName(args config.Args) string {
	if isInteractiveQueryMode(args) {
		return "query"
	}
	return args.Mode.String()
}

// completeQueryCommand completes the word at the end of before, the line up
// to the cursor, inside the query of a :query line, with the query
// completer of --query values (completeQueryWord). The query is the rest of
// the line as typed, so candidates are inserted as they are, without the
// shell escaping of a quoted --query value; after, the line behind the
// cursor, is the rest of the query, whose clauses and from table guide the
// completion too. It reports false for any other line and in modes without
// :query.
func completeQueryCommand(unchanged completion, before, after string, args config.Args,
	catalog func() catalogSnapshot) (completion, bool) {

	trimmed := strings.TrimLeftFunc(before, unicode.IsSpace)
	rest, found := strings.CutPrefix(trimmed, queryCommand)
	if !found || rest == "" || (rest[0] != ' ' && rest[0] != '\t') || !hasQueryCommand(args) {
		return completion{}, false
	}
	query := completeQueryWord(strings.TrimLeftFunc(rest, unicode.IsSpace), after, catalog())
	replacement, listed := query.insertion()
	result := query.listInto(unchanged, listed)
	if replacement != "" {
		result.line = before[:len(before)-len(query.word)] + replacement + after
		result.pos = len(result.line) - len(after)
		result.changed = result.line != unchanged.line
		result.itemBlank = query.itemBlankSize(replacement)
	}
	return result, true
}
