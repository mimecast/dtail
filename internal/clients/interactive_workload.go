package clients

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

var (
	// errWorkloadSwitchUnsupported rejects a switch between a tail read and a
	// query in a client that cannot hold both: only interactive dtail can
	// (see NewInteractiveTailClient).
	errWorkloadSwitchUnsupported = errors.New("only an interactive dtail session can switch between " +
		"following lines and a MapReduce query")
	// errAlreadyTailing rejects a switch to the tail read without a regex
	// while the session already follows lines.
	errAlreadyTailing = errors.New("the session already follows lines")
	// errBlankQuery rejects a switch to a blank query, which would be no
	// query at all.
	errBlankQuery = errors.New("the query must not be blank")
)

// invertSetAsideNote tells the user that the --invert of the lines followed
// before does not apply to the query; it applies again once the session
// follows lines.
const invertSetAsideNote = "--invert is set aside while the query runs: " + queryInvertReason

// tailFilter is the line filter of a tail workload that a switch to a query
// sets aside, so that the switch back follows the lines it followed before:
// the query reads the lines of its table (applyQueryRegex), never inverted,
// and every line of them, so it has no use for the tail's regex, --invert,
// context lines or --max (a --max would stop the query's reads after so
// many lines, context lines would feed it lines of other tables).
type tailFilter struct {
	regex   string
	invert  bool
	before  int
	after   int
	max     int
	defined bool
}

// tailFilterOf returns the line filter of args, a tail workload.
func tailFilterOf(args config.Args) tailFilter {
	return tailFilter{
		regex:   args.RegexStr,
		invert:  args.RegexInvert,
		before:  args.BeforeContext,
		after:   args.AfterContext,
		max:     args.MaxCount,
		defined: true,
	}
}

// applyTo sets the filter on args. An undefined filter (the session started
// with a query and no --regex) follows every line.
func (f tailFilter) applyTo(args *config.Args) {
	if !f.defined {
		f.regex = "."
	}
	args.RegexStr = f.regex
	args.RegexInvert = f.invert
	args.BeforeContext = f.before
	args.AfterContext = f.after
	args.MaxCount = f.max
}

// clearTailFilter drops the tail-only parts of the line filter from args of
// a query workload (see tailFilter); applyQueryRegex sets its regex.
func clearTailFilter(args *config.Args) {
	args.RegexInvert = false
	args.BeforeContext = 0
	args.AfterContext = 0
	args.MaxCount = 0
}

// canSwitchWorkload reports whether the client can switch its running
// session between a tail read and a MapReduce query: interactive dtail
// (clientProfile.switchWorkload, a MaprClient whose query is optional, see
// NewInteractiveTailClient). dmap always runs a query and dcat/dgrep never
// do.
func (c *baseClient) canSwitchWorkload() bool {
	args, _ := c.snapshotConnectionState()
	return c.profile.switchWorkload && args.Mode == omode.TailClient && args.InteractiveQuery
}

// switchToQuery switches the running session of every connection to the
// MapReduce query query, with the files and options it has, like a :reload
// --query does (applyInteractiveReload: all or nothing, rolled back on a
// failure, the client's aggregation switched by the profile commit,
// MaprClient.commitSessionSpec). Coming from the tail read, its line filter
// is set aside for switchToTail. In query mode it replaces the query. On an
// error (blank or invalid query, a failed update) the running workload is
// left as it was.
//
// The caller resumes paused output first, as for a :reload: the servers'
// acknowledgements are read by the handlers the pause blocks.
func (c *baseClient) switchToQuery(query string) (reloadOutcome, error) { //nolint:unparam // The :query command reports the outcome.
	if !c.canSwitchWorkload() {
		return reloadOutcome{}, errWorkloadSwitchUnsupported
	}
	if strings.TrimSpace(query) == "" {
		return reloadOutcome{}, errBlankQuery
	}
	current, _ := c.snapshotConnectionState()
	saved := c.savedTailFilter()
	if !isInteractiveQueryMode(current) {
		saved = tailFilterOf(current)
	}
	next := current
	next.QueryStr = query
	clearTailFilter(&next)
	// The query's first aggregates may arrive before the commit; the
	// handlers take frames for aggregate data only while they are expected
	// (see clientProfile.expectQuery).
	c.expectQuery(true)
	outcome, err := c.switchWorkload(next, saved)
	if err != nil {
		c.expectQuery(false)
		return reloadOutcome{}, err
	}
	if current.RegexInvert && !isInteractiveQueryMode(current) {
		outcome.notes = append(outcome.notes, invertSetAsideNote)
	}
	return outcome, nil
}

// expectQuery passes a pending switch to a query (true) or its failure
// (false) on to the profile, if it takes it.
func (c *baseClient) expectQuery(pending bool) {
	if c.profile.expectQuery != nil {
		c.profile.expectQuery(pending)
	}
}

// switchToTail switches the running session of every connection from its
// MapReduce query back to following lines, with the line filter the tail
// workload had before the switch to the query (see tailFilter). A non-empty
// regex replaces its regex and clears --invert, as a regex given with the
// switch names the lines to follow; context lines and --max are kept. While
// the session already follows lines, a regex replaces its regex the same way
// and no regex is errAlreadyTailing. Errors leave the running workload as it
// was; the caller resumes paused output first (see switchToQuery).
func (c *baseClient) switchToTail(regex string) (reloadOutcome, error) { //nolint:unparam // The :tail command reports the outcome.
	if !c.canSwitchWorkload() {
		return reloadOutcome{}, errWorkloadSwitchUnsupported
	}
	current, _ := c.snapshotConnectionState()
	if !isInteractiveQueryMode(current) && regex == "" {
		return reloadOutcome{}, errAlreadyTailing
	}
	next := current
	saved := c.savedTailFilter()
	if isInteractiveQueryMode(current) {
		saved.applyTo(&next)
	}
	next.QueryStr = ""
	if regex != "" {
		next.RegexStr = regex
		next.RegexInvert = false
	}
	return c.switchWorkload(next, tailFilterOf(next))
}

// switchWorkload builds the session spec of next and applies it to every
// connection (applyInteractiveReload); once applied, saved is the tail
// filter a later switchToTail restores. The :query and :tail commands built
// the same spec while their line was parsed (checkQuerySpec,
// parseTailCommand), before paused output was resumed, so the build here
// fails only for a caller that did not.
func (c *baseClient) switchWorkload(next config.Args, saved tailFilter) (reloadOutcome, error) {
	next, spec, err := buildInteractiveSessionSpec(next)
	if err != nil {
		return reloadOutcome{}, fmt.Errorf("invalid workload: %w", err)
	}
	outcome, err := c.applyInteractiveReload(next, spec)
	if err != nil {
		return reloadOutcome{}, err
	}
	c.storeTailFilter(saved)
	return outcome, nil
}

// savedTailFilter returns the tail filter a switch to a query set aside.
func (c *baseClient) savedTailFilter() tailFilter {
	mu := c.stateMu()
	mu.RLock()
	defer mu.RUnlock()
	return c.tailFilter
}

// storeTailFilter stores the tail filter a switch to a query set aside.
func (c *baseClient) storeTailFilter(filter tailFilter) {
	mu := c.stateMu()
	mu.Lock()
	defer mu.Unlock()
	c.tailFilter = filter
}

// queryFailureMarks records, per session handler, how many failed commands
// its server had reported when interactive dtail switched its session from
// following lines to a query, so that the query's incomplete-result warnings
// (MaprClient.incompleteResultReasons) leave out the failures of the tail
// workloads before it. A handler of a connection made later (a reconnect) has
// no mark: all of its failures count. The zero value marks nothing.
type queryFailureMarks struct {
	mu    sync.Mutex
	marks map[handlers.Handler]int
}

// mark records the failures reported so far on the handlers of connections,
// replacing earlier marks.
func (m *queryFailureMarks) mark(connections []connectors.Connector) {
	marks := make(map[handlers.Handler]int, len(connections))
	for _, conn := range connections {
		handler := conn.Handler()
		if !markable(handler) {
			continue
		}
		if reporter, ok := handler.(sessionOutcomeReporter); ok {
			marks[handler] = len(reporter.Outcome().Failures)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marks = marks
}

// since returns the failures of handler reported after its mark.
func (m *queryFailureMarks) since(handler handlers.Handler, failures []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !markable(handler) {
		return failures
	}
	return failures[min(m.marks[handler], len(failures)):]
}

// markable reports whether handler can be a key of the marks: a nil handler
// or one of a type that is not comparable (no handler of the clients, which
// are pointers) would make no sense or panic as a map key.
func markable(handler handlers.Handler) bool {
	return handler != nil && reflect.TypeOf(handler).Comparable()
}
