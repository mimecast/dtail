package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
)

// MaprClientMode determines whether to use cumulative mode or not.
type MaprClientMode int

const (
	// DefaultMode behaviour
	DefaultMode MaprClientMode = iota
	// CumulativeMode means results are added to prev interval
	CumulativeMode MaprClientMode = iota
	// NonCumulativeMode means results are from 0 for each interval
	NonCumulativeMode MaprClientMode = iota
	// ScheduledMode is CumulativeMode for dserver's scheduled jobs: the result
	// is written once, after the query completed on every server, and never
	// when it failed or was canceled (see Start).
	ScheduledMode MaprClientMode = iota
	// ScheduledPartialMode is ScheduledMode for the run of a scheduled job
	// after its TimeRange ended: it also writes the result when the only
	// failures were files that were missing, not permitted or unreadable
	// (see Start).
	ScheduledPartialMode MaprClientMode = iota
)

// MaprClient is used for running mapreduce aggregations on remote files.
type MaprClient struct {
	baseClient
	// Shared mapreduce state for all handlers and reporting paths.
	session *maprclient.SessionState
	// resultLog is the logger of session and of everything logged with one
	// of its locks held (lockedLogger): it never blocks, unlike the client
	// logger while the output is paused (see deferredLogger). Nil in
	// tests that build the client without it.
	resultLog *deferredLogger
	// Selected cumulative reporting mode.
	mode MaprClientMode
	// Publication state prevents an empty interval from replacing a result
	// produced earlier in the same query generation.
	outfileState outfileReportState
	// queryMarks are the failures every server reported before interactive
	// dtail switched its session to the active query, which are not the
	// query's (see incompleteResultReasons).
	queryMarks queryFailureMarks
}

type outfileReportState struct {
	mu         sync.Mutex
	generation uint64
	hasRows    bool
}

// NewMaprClient returns a new mapreduce client.
func NewMaprClient(args config.Args, cfg config.RuntimeConfig, maprClientMode MaprClientMode,
	loggers LoggerDependencies, colorizers ...*brush.Brush) (*MaprClient, error) {
	if args.QueryStr == "" {
		return nil, errors.New("no mapreduce query specified, use '-query' flag")
	}
	return newMaprClient(args, cfg, maprClientMode, loggers, firstColorizer(colorizers), false)
}

// NewInteractiveTailClient returns the client of an interactive dtail
// (--interactive-query, dtail's default on a terminal): a MapReduce client
// in tail mode whose query is optional, so that its running session can
// switch between following lines (no query, args.QueryStr empty) and a
// MapReduce query, both ways, with SESSION UPDATE (switchToQuery,
// switchToTail). Its handlers are MaprHandlers, which print every payload
// line and aggregate the aggregate frames; results are reported only while
// a query is active. A --plain line that starts like an aggregate frame
// (protocol.CutAggregateContent) is printed while the session follows lines;
// while a query runs or a switch to one is pending
// (maprclient.SessionState.AggregatesExpected) it is taken for aggregate
// data, as with dtail --query. It keeps the exit statuses of dtail's follow
// mode, as TailClient and the MaprClient of dtail --query do (no
// failed-command or cut-short status). A non-interactive dtail keeps
// TailClient (no --query) or NewMaprClient (--query).
func NewInteractiveTailClient(args config.Args, cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizers ...*brush.Brush) (*MaprClient, error) {
	if !args.InteractiveQuery {
		return nil, errors.New("the switchable dtail client needs --interactive-query")
	}
	args.Mode = omode.TailClient
	return newMaprClient(args, cfg, DefaultMode, loggers, firstColorizer(colorizers), true)
}

// newMaprClient returns a mapreduce client for args. With switchable (see
// NewInteractiveTailClient) the query may be empty, and the line filter of
// the tail read a query replaces is set aside for the switch back
// (tailFilter): the query then reads without the tail's --invert, context
// lines and --max.
func newMaprClient(args config.Args, cfg config.RuntimeConfig, maprClientMode MaprClientMode,
	loggers LoggerDependencies, colorizer *brush.Brush, switchable bool) (*MaprClient, error) {
	loggers = loggers.normalized()
	query, saved, err := prepareMaprArgs(&args, loggers, switchable)
	if err != nil {
		return nil, err
	}
	// Reconnect only in tail mode and without an outfile. A switchable
	// client keeps what it was created with when it switches later.
	retry := args.Mode == omode.TailClient && (query == nil || !query.HasOutfile())

	session, resultLog := newMaprSession(query, loggers.Client)
	c := MaprClient{
		session:   session,
		resultLog: resultLog,
		mode:      maprClientMode,
	}
	// Only the command-line clients (DefaultMode) may run interactively.
	// dserver's scheduled and continuous jobs (every other mode) have no
	// terminal: an interactive session would never end by itself and the
	// control loop would fail to open the control TTY.
	if maprClientMode != DefaultMode {
		args.InteractiveQuery = false
	}

	base, initErr := newBaseClient(args, cfg, loggers, colorizer,
		maprClientProfile(&c, loggers, retry, switchable))
	if initErr != nil {
		return nil, fmt.Errorf("initialize mapreduce client: %w", initErr)
	}
	c.baseClient = base
	c.tailFilter = saved
	loggers.Client.Debug("Cumulative mapreduce mode?", c.isCumulative(query))

	return &c, nil
}

// maprClientProfile returns the profile of the mapreduce client c: its
// connections get MaprHandlers aggregating into c's session state, and an
// interactive reload commits its spec to that state (commitSessionSpec),
// which is told about the reload from its start on, so that no aggregate
// of the reloaded query that arrives before the commit is lost.
func maprClientProfile(c *MaprClient, loggers LoggerDependencies, retry, switchable bool) clientProfile {
	return clientProfile{
		newHandler: func(server string) handlers.Handler {
			return handlers.NewMaprHandler(server, c.session, loggers.Client)
		},
		retry:          retry,
		commit:         c.commitSessionSpec,
		switchWorkload: switchable,
		expectQuery:    c.session.ExpectAggregates,
		reloading:      c.session.Switching,
	}
}

// prepareMaprArgs parses the query of args, nil for a switchable client
// without one, and sets the query's line filter on args (applyQueryRegex).
// For a switchable client with a query it returns the tail filter of args
// (see tailFilter), cleared from args; otherwise an undefined one.
func prepareMaprArgs(args *config.Args, loggers LoggerDependencies, switchable bool) (*mapr.Query,
	tailFilter, error) {
	if switchable && args.QueryStr == "" {
		return nil, tailFilter{}, nil
	}
	query, err := mapr.NewQuery(args.QueryStr, loggers.Client)
	if err != nil {
		return nil, tailFilter{}, fmt.Errorf("parse mapreduce query %q: %w", args.QueryStr, err)
	}

	// Warn once, at plan time, about $-variables the selected parser cannot
	// populate. This runs in the user's client process for both server and
	// serverless mode, so the warning reaches the user's stderr even though the
	// actual field resolution happens server-side.
	warnUnknownQueryVariables(os.Stderr, query, loggers.Client)
	var saved tailFilter
	if switchable {
		// The --invert applies to the tail read a switch goes back to.
		saved = tailFilterOf(*args)
		clearTailFilter(args)
		if saved.invert {
			if _, err := fmt.Fprintln(os.Stderr, "NOTE: "+invertSetAsideNote); err != nil {
				loggers.Client.Debug("Unable to write invert note", err)
			}
		}
	} else {
		warnIgnoredInvert(os.Stderr, *args, loggers.Client)
	}
	applyQueryRegex(args, query)
	return query, saved, nil
}

// Start starts the mapreduce client.
//
// After connections finish, Start stops and joins the periodic reporter before
// publishing the final result. Non-cumulative clients publish their remaining
// interval even on cancellation; an empty interval preserves any non-empty
// outfile already published in that query generation.
//
// In the scheduled modes no interim result is written, and the final result
// is written only when the query completed (see scheduledQueryOutcome): ctx
// was not canceled and on every server the session ended with status 0, with
// the server's close handshake and without a failed command. In
// ScheduledPartialMode a query whose only failures are file read failures
// (protocol.IsFileReadFailure) writes the result it has too. Start then
// returns 0 if and only if the final result was written, so a failed
// scheduled job leaves no outfile (and no .query file) and the scheduler runs
// it again later. An outfile from an earlier run is left untouched.
func (c *MaprClient) Start(ctx context.Context, statsCh <-chan string) (status int) {
	reportCtx, stopReporting := context.WithCancel(ctx)
	defer stopReporting()
	var reporter sync.WaitGroup
	if !c.mode.scheduled() {
		reporter.Add(1)
		go func() {
			defer reporter.Done()
			c.periodicReportResults(reportCtx)
		}()
	}

	status = c.baseClient.Start(ctx, statsCh)
	// Connections have drained all received aggregates. Stop and join any
	// in-flight report before publishing the remaining interval, also when
	// connections completed without canceling the caller's context.
	stopReporting()
	reporter.Wait()
	return c.finish(ctx.Err(), status)
}

// awaitReload waits for an interactive reload in flight, if any, to end. A
// reload holds the aggregates the servers send after their acknowledgements
// and merges them when it ends (see maprclient.SessionState.Switching), so
// a final result read before that would lack them. With the sessions ended
// the reload does not take long: an acknowledgement wait ends with its
// session.
func (c *MaprClient) awaitReload() {
	reloadMu := c.reloadLock()
	reloadMu.Lock()
	defer reloadMu.Unlock()
}

// scheduled reports whether mode is one of dserver's scheduled job modes.
func (mode MaprClientMode) scheduled() bool {
	return mode == ScheduledMode || mode == ScheduledPartialMode
}

// finish publishes the final result after the connections and reporter ended,
// consuming the remaining interval for a non-cumulative query. ctxErr is the
// error of the client's context by then; only the scheduled modes use it
// (interactive dmap decides per session whether the client ended it, see
// incompleteResultReasons). As for interim reports, output errors
// are logged; only scheduled modes change the exit status on publication errors.
//
// Outside the scheduled modes neither a failed command a server reported nor
// a session cut short changes the exit status or keeps the result from being
// written (dmap opts out of the failed command and cut-short statuses, see
// clientProfile.failedCommandStatus and cutShortStatus, so that it always
// writes what it received). Every one of them that may have lost data (see
// incompleteResultReasons) is logged at WARN first instead, so the user
// learns that the result may lack data.
func (c *MaprClient) finish(ctxErr error, status int) int {
	defer c.flushResultLog()
	if c.mode.scheduled() {
		return c.finishScheduled(ctxErr, status)
	}
	c.awaitReload()
	// What a reload printed without blocking comes before the final result.
	c.flushResultLog()
	if c.session.Snapshot().Query == nil {
		// Interactive dtail ended while following lines: no query, no
		// result, nothing to warn about (see dropQuery).
		return status
	}

	if reasons := c.incompleteResultReasons(); len(reasons) > 0 {
		c.clientLogger().Warn("The mapreduce result may be incomplete", strings.Join(reasons, "; "))
	}
	// Publish already-received data even if the connections were canceled.
	c.clientLogger().Debug("Writing final mapreduce result")
	if err := c.reportResults(true); err != nil {
		c.clientLogger().Error("Unable to write final mapreduce result", err)
		return status
	}
	c.clientLogger().Debug("Final result written")
	return status
}

// lockedLogger returns the logger for diagnostics logged while a lock of
// the session state or of a group set is held: one that never blocks.
func (c *MaprClient) lockedLogger() logging.Logger {
	if c.resultLog == nil {
		return c.clientLogger()
	}
	return c.resultLog
}

// flushResultLog waits for the diagnostics of the mapreduce state to be
// logged, so that none is lost when the client ends right after its final
// result (e.g. the lines naming the outfile written).
func (c *MaprClient) flushResultLog() {
	if c.resultLog != nil {
		c.resultLog.flush()
	}
}

// controlLogger returns the logger for what the interactive control loop
// prints while it commits a reload (commitSessionSpec): the last table of a
// query that ends, and the errors of writing it. The control loop alone
// answers the host key question, which pauses the client logger, so it
// must never wait for that logger: with the question asked during a
// reload, a :tail (dropQuery printing the query's last table) or the
// reload's end left it in the paused logger for good, no lock involved.
// The reporter and finish keep the client logger and so stay synchronous.
func (c *MaprClient) controlLogger() clientlog.Logger {
	if c.resultLog == nil {
		return c.clientLogger()
	}
	return c.resultLog
}

func (c *MaprClient) finishScheduled(ctxErr error, status int) int {
	outcome := c.scheduledQueryOutcome(ctxErr, status)
	failures := strings.Join(outcome.fileFailures, "; ")
	switch {
	case outcome.incomplete != "":
		c.clientLogger().Warn("Not writing the mapreduce result as the query did not complete", outcome.incomplete)
		return max(status, 1)
	case failures != "" && c.mode != ScheduledPartialMode:
		c.clientLogger().Warn("Not writing the mapreduce result as files could not be read "+
			"(a run after the job's TimeRange ended writes what it could read)", failures)
		return max(status, 1)
	case failures != "":
		c.clientLogger().Warn("Writing partial mapreduce result after the job's TimeRange ended", failures)
	}
	c.clientLogger().Debug("Writing final mapreduce result")
	if err := c.reportResults(true); err != nil {
		c.clientLogger().Error("Unable to write final mapreduce result", err)
		return 1
	}
	c.clientLogger().Debug("Final result written")
	return status
}

// scheduledOutcome is how a scheduled query ended.
type scheduledOutcome struct {
	// incomplete is why the query did not complete, or "" if it did: a
	// connection failed or was canceled, a session was cut short, or a server
	// reported a failed command that is no file read failure. Such a query
	// never writes a result.
	incomplete string
	// fileFailures are the file read failures (protocol.IsFileReadFailure)
	// the servers reported, each as "<server>: <reason>": files that were
	// missing, not permitted or unreadable, so the result lacks their data.
	fileFailures []string
}

// scheduledQueryOutcome returns how the scheduled query ended: it did not
// complete unless the connections ended with status 0, ctxErr (the error of
// the client's context by then) is nil, there was a connection at all, and
// every session completed (see sessionOutcome). A client whose discovery
// yields no server already fails when it is created (ErrNoServers), which
// the scheduler treats as a failed run as well; the check for no connection
// stays as a guard, so that a query that ran nowhere never writes a result.
func (c *MaprClient) scheduledQueryOutcome(ctxErr error, status int) scheduledOutcome {
	if reason := incompleteQueryReason(ctxErr, status); reason != "" {
		return scheduledOutcome{incomplete: reason}
	}
	connections := c.snapshotConnections()
	if len(connections) == 0 {
		return scheduledOutcome{incomplete: "there was no server to run the query on"}
	}
	var outcome scheduledOutcome
	for _, conn := range connections {
		incomplete, fileFailures := sessionOutcome(conn.Server(), conn.Handler())
		if incomplete != "" {
			return scheduledOutcome{incomplete: incomplete}
		}
		outcome.fileFailures = append(outcome.fileFailures, fileFailures...)
	}
	return outcome
}

// sessionOutcome returns why the session of handler with server did not
// complete, or "" if it did, and the file read failures (see
// protocol.IsFileReadFailure) the server reported, each as "<server>:
// <reason>". The session completed when the server ended it with its close
// handshake (so it was not cut short, e.g. by a killed or shut down server or
// a lost connection) and reported no failed command other than file read
// failures.
func sessionOutcome(server string, handler handlers.Handler) (incomplete string, fileFailures []string) {
	reporter, ok := handler.(sessionOutcomeReporter)
	if !ok {
		return fmt.Sprintf("the session with %s reports no outcome", server), nil
	}
	outcome := reporter.Outcome()
	if !outcome.Completed {
		return fmt.Sprintf("the session with %s %s", server, sessionCutShortReason), nil
	}
	for _, reason := range outcome.Failures {
		if !protocol.IsFileReadFailure(reason) {
			return fmt.Sprintf("%s reported a failed command: %s", server, reason), nil
		}
		fileFailures = append(fileFailures, server+": "+reason)
	}
	return "", fileFailures
}

// sessionCutShortReason is why a session that ended without the server's
// close handshake (handlers.SessionOutcome Completed is false) may lack data.
const sessionCutShortReason = "ended before the server completed it"

// incompleteResultReasons returns, each as "<server>: <reason>", why the
// result of an interactive dmap may lack data: every failed command the
// servers reported that is no file read failure (see
// protocol.IsFileReadFailure), e.g. "map: incomplete result", and every
// session cut short, e.g. by the idle timeout, a dserver shut down with
// SIGTERM or a lost connection, while it sent the final result, unless the
// client ended that session itself. Whether it did is decided per session at
// the moment the session ended (see baseClient.runSession): SIGINT/SIGTERM
// cancel the context of Start, and :quit and the end of the control TTY of
// an --interactive-query session only the context of the connections, so the
// context of Start cannot tell; and a session cut short before a later
// cancel by the client still counts. A session cut short has no "map:
// incomplete result" report, as the server could not send it any more.
// Interactive dmap does not fail on these, it only warns (see finish). File
// read failures are left out: they mean that a file had no data to give (it
// was missing, not permitted or unreadable), not that a command was rejected
// or data the server read was lost on the way. A handler that cannot tell
// how its session ended is left out too, and so are the failures a server
// reported before interactive dtail switched its session from following
// lines to the active query (queryMarks).
func (c *MaprClient) incompleteResultReasons() []string {
	var reasons []string
	for _, session := range c.snapshotSessions() {
		conn := session.conn
		reporter, ok := conn.Handler().(sessionOutcomeReporter)
		if !ok {
			continue
		}
		outcome := reporter.Outcome()
		if !outcome.Completed && !session.endedByClient {
			reasons = append(reasons, conn.Server()+": the session "+sessionCutShortReason)
		}
		for _, reason := range c.queryMarks.since(conn.Handler(), outcome.Failures) {
			if !protocol.IsFileReadFailure(reason) {
				reasons = append(reasons, conn.Server()+": "+reason)
			}
		}
	}
	return reasons
}

// incompleteQueryReason returns why a query whose connections ended with
// status, while its context had ctxErr, did not complete, or "" if it did.
func incompleteQueryReason(ctxErr error, status int) string {
	switch {
	case status != 0:
		return fmt.Sprintf("a server connection ended with status %d", status)
	case ctxErr != nil:
		return fmt.Sprintf("the query was canceled: %v", ctxErr)
	default:
		return ""
	}
}

// periodicReportResults reports the interim results (prints them or writes
// the outfile) until ctx is done, one query generation at a time
// (reportGeneration). It does not run in the scheduled modes.
func (c *MaprClient) periodicReportResults(ctx context.Context) {
	for ctx.Err() == nil {
		snapshot := c.session.Snapshot()
		if snapshot.Query == nil {
			// Interactive dtail follows lines: nothing to report (and to
			// log about) until a query is committed.
			if !c.awaitQueryChange(ctx) {
				return
			}
			continue
		}
		if !c.reportGeneration(ctx, snapshot) {
			return
		}
	}
}

// reportGeneration reports the interim results of snapshot's query
// generation until the session state changes (true) or ctx is done (false).
//
// The first report comes half an interval after the generation started.
// Each dserver sends its first aggregates only one interval after it started
// the query, though, so that report has nothing to show (an empty table is
// not printed) and, on a fixed cadence, the first table would follow only
// 1.5 intervals after the start. So, except for continuous jobs
// (earlyFirstReport), the servers' first aggregates merged for the
// generation (SessionSnapshot.AggregatesArrived) move the next report to
// shortly after them (firstReportWindow: firstReportSettle after the last
// live server's, or, while one is missing and its connection is up, half an
// interval after the first server's; firstReportWindow.postpone). Every
// report after it is planned one interval after the instant the previous
// one was planned for (reportSchedule, so the time a report takes does not
// add up), which keeps the reports between the servers' sends, at most half
// an interval behind them. A table then normally holds one send of every
// server; it is no guarantee, as each server sends by its own clock, one
// interval after it started the query, and a send takes its time to arrive
// and to be merged.
func (c *MaprClient) reportGeneration(ctx context.Context, snapshot maprclient.SessionSnapshot) bool {
	interval := c.reportDelay(snapshot.Query, false)
	window := c.newFirstReportWindow(snapshot, interval)
	allArrived := func() bool { return c.allLiveArrived(snapshot) }
	delay := c.reportDelay(snapshot.Query, true)
	c.clientLogger().Debug("Sleeping before processing mapreduce results", "generation", snapshot.Generation, "delay", delay)
	schedule := newReportSchedule(time.Now(), delay)
	defer schedule.timer.Stop()

	for {
		select {
		case <-window.arrived:
			all := allArrived()
			now := time.Now()
			schedule.in(now, window.settle(now, all))
			c.clientLogger().Debug("Mapreduce aggregates of a server arrived", "generation", snapshot.Generation,
				"allLiveServers", all)
		case <-schedule.timer.C:
			if now := time.Now(); c.postponeReport(window, schedule, now, allArrived) {
				continue
			}
			window.reporting()
			c.clientLogger().Debug("Gathering interim mapreduce result")
			if err := c.reportResults(false); err != nil {
				c.clientLogger().Error("Unable to gather mapreduce result", err)
			}
			schedule.next(time.Now(), interval)
		case <-c.session.Changes():
			c.clientLogger().Debug("Mapreduce query generation changed, recalculating report interval")
			return true
		case <-ctx.Done():
			return false
		}
	}
}

// postponeReport puts the report due at now off if the first report's
// window says so (firstReportWindow.postpone) and reports whether it did.
func (c *MaprClient) postponeReport(window *firstReportWindow, schedule *reportSchedule, now time.Time,
	allArrived func() bool) bool {
	wait, ok := window.postpone(now, allArrived)
	if !ok {
		return false
	}
	schedule.in(now, wait)
	c.clientLogger().Debug("Mapreduce report put off for the first aggregates of the live servers", "wait", wait)
	return true
}

// awaitQueryChange waits until the session state changes (a query is
// committed or cleared) and reports false once ctx is done.
func (c *MaprClient) awaitQueryChange(ctx context.Context) bool {
	select {
	case <-c.session.Changes():
		return true
	case <-ctx.Done():
		return false
	}
}

// reportResults reports the current result (prints it or writes the
// outfile), printing and logging synchronously to the client logger.
func (c *MaprClient) reportResults(finalResult bool) error {
	return c.reportResultsTo(c.clientLogger(), finalResult)
}

// reportResultsTo is reportResults printing and logging to out: the
// client logger, or controlLogger on the control loop.
func (c *MaprClient) reportResultsTo(out clientlog.Logger, finalResult bool) error {
	snapshot := c.session.Snapshot()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		return nil
	}

	if snapshot.Query.HasOutfile() {
		return c.writeResultsToOutfile(out, snapshot, finalResult)
	}
	return c.printResults(out, snapshot)
}

// printResults renders the result of snapshot and prints it to out, unless
// it has no rows or did not change since the last print. The table is
// rendered here in any case; only its output may be deferred (out).
func (c *MaprClient) printResults(out clientlog.Logger, snapshot maprclient.SessionSnapshot) error {
	var result string
	var err error
	var numRows int
	rowsLimit := -1

	if snapshot.Query.Limit == -1 {
		// Limit output to 10 rows when the result is printed to stdout.
		// This can be overriden with the limit clause though.
		rowsLimit = 10
	}

	if c.isCumulative(snapshot.Query) {
		result, numRows, err = snapshot.GlobalGroup.Result(snapshot.Query, rowsLimit, c.runtime.output.MaprResultRenderer())
	} else {
		result, numRows, err = snapshot.GlobalGroup.SwapOut().Result(snapshot.Query, rowsLimit, c.runtime.output.MaprResultRenderer())
	}
	if err != nil {
		return fmt.Errorf("unable to render mapreduce result: %w", err)
	}

	changed, ok := c.session.CommitRenderedResult(snapshot.Generation, result)
	if !ok {
		out.Debug("Discarding stale mapreduce result", "generation", snapshot.Generation)
		return nil
	}
	if !changed {
		out.Debug("Result hasn't changed compared to last time...")
		return nil
	}

	if numRows == 0 {
		out.Debug("Empty result set this time...")
		return nil
	}

	rawQuery := c.runtime.output.PaintMaprRawQuery(snapshot.Query.RawQuery)
	clientlog.Raw(out, fmt.Sprintf("%s\n", rawQuery))

	if rowsLimit > 0 && numRows > rowsLimit {
		out.Warn(fmt.Sprintf("Got %d results but limited terminal output "+
			"to %d rows! Use 'limit' clause to override!", numRows, rowsLimit))
	}
	clientlog.Raw(out, fmt.Sprintf("%s\n", result))
	return nil
}

func (c *MaprClient) writeResultsToOutfile(out clientlog.Logger, snapshot maprclient.SessionSnapshot,
	finalResult bool) error {
	current, err := c.session.WithCurrentSnapshot(snapshot, func() error {
		// Lock order inside the callback is SessionState read lock, then
		// outfileState.mu, then the aggregate/file locks. Do not re-enter
		// SessionState from this callback, and log only through
		// lockedLogger in it: the client logger blocks while the output
		// is paused, which would keep these locks from a :reload
		// (deferredLogger).
		return c.writeCurrentResultsToOutfile(snapshot, finalResult)
	})
	if !current {
		out.Debug("Discarding stale mapreduce outfile result", "generation", snapshot.Generation)
		return nil
	}
	return err
}

func (c *MaprClient) writeCurrentResultsToOutfile(snapshot maprclient.SessionSnapshot, finalResult bool) error {
	cumulative := c.isCumulative(snapshot.Query)
	c.lockedLogger().Debug("writeResultsToOutfile called", "finalResult", finalResult, "cumulative", cumulative, "generation", snapshot.Generation)
	if cumulative {
		return c.writeCumulativeResult(snapshot, finalResult)
	}

	if snapshot.Query.Outfile.AppendMode {
		// Each interval's rows are swapped out, so every report appends
		// only what arrived since the previous one, once.
		group := snapshot.GlobalGroup.SwapOut()
		if err := group.WriteResult(snapshot.Query, true); err != nil {
			return fmt.Errorf("unable to write non-cumulative mapreduce result: %w", err)
		}
		c.lockedLogger().Debug("WriteResult completed for non-cumulative append mode")
		return nil
	}

	// Keep detaching and publishing in one critical section so overlapping
	// reports cannot write older intervals after newer ones.
	c.outfileState.mu.Lock()
	defer c.outfileState.mu.Unlock()

	group := snapshot.GlobalGroup.SwapOut()
	empty := group.IsEmpty()
	if c.outfileState.generation != snapshot.Generation {
		// An outfile from an earlier generation is stale. Leave hasRows false
		// so the first empty interval replaces it with this query's header.
		c.outfileState.generation = snapshot.Generation
		c.outfileState.hasRows = false
	}
	if empty && c.outfileState.hasRows {
		c.lockedLogger().Debug("Preserving previous non-empty outfile result", "generation", snapshot.Generation)
		return nil
	}
	if err := group.WriteResult(snapshot.Query, true); err != nil {
		return fmt.Errorf("unable to write non-cumulative mapreduce result: %w", err)
	}
	if !empty {
		c.outfileState.hasRows = true
	}
	c.lockedLogger().Debug("WriteResult completed for non-cumulative mode")
	return nil
}

// writeCumulativeResult writes the whole result so far of a cumulative
// query. Without append every report replaces the outfile (via .tmp and
// rename), so interim reports keep it up to date. With "outfile append" an
// interim report appends no rows: the result so far contains every earlier
// report's rows again, so appending it would repeat them on every interval.
// It only checks that the outfile can be opened for appending
// (checkAppendOutfile), so an unwritable path is reported every interval
// instead of only when the final write fails at the end of the run. Only the
// final result of the query is appended, once (finish, or dropQuery and
// commitSessionSpec when a reload ends the query). Until then the result
// lives only in memory.
func (c *MaprClient) writeCumulativeResult(snapshot maprclient.SessionSnapshot, finalResult bool) error {
	if snapshot.Query.Outfile.AppendMode {
		if !finalResult {
			c.lockedLogger().Debug("Not appending interim cumulative mapreduce result", "generation", snapshot.Generation)
			return checkAppendOutfile(snapshot.Query.Outfile.FilePath)
		}
		// Serializes with appendReplacedResult, which writes a replaced
		// generation's result outside of the session's read lock.
		c.outfileState.mu.Lock()
		defer c.outfileState.mu.Unlock()
	}
	if err := snapshot.GlobalGroup.WriteResult(snapshot.Query, finalResult); err != nil {
		return fmt.Errorf("unable to write cumulative mapreduce result: %w", err)
	}
	c.lockedLogger().Debug("WriteResult completed for cumulative mode")
	return nil
}

// checkAppendOutfile opens path for appending, as the final write will, and
// closes it again without writing. It creates path when it is missing (an
// empty file, to which the final result then appends its CSV header), so a
// missing directory, a denied permission or a directory at path fail here.
func checkAppendOutfile(path string) error {
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return fmt.Errorf("unable to open mapreduce outfile for appending: %w", err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("unable to close mapreduce outfile %q: %w", path, err)
	}
	return nil
}

// commitSessionSpec is the profile commit of an interactive reload: it
// switches the client-side aggregation to the reloaded query at the new
// generation. It does not touch c.Args: reconnects read them concurrently
// under the state lock, and applyInteractiveReload stores the reloaded args
// (with the query and its normalized line filter, see
// normalizeInteractiveArgs) under that lock right after this commit.
//
// A spec without a query is rejected, except by the switchable client of
// interactive dtail (clientProfile.switchWorkload), whose session then
// follows lines: see dropQuery. A query that follows none starts with the
// failures the servers reported so far marked as not its own (queryMarks).
// A replaced cumulative "outfile append" query gets its result appended once
// (appendReplacedResult), as its interim reports appended nothing.
func (c *MaprClient) commitSessionSpec(spec SessionSpec, generation uint64) error {
	if spec.Query == "" {
		if !c.profile.switchWorkload {
			return errors.New("missing mapreduce query")
		}
		c.dropQuery(generation)
		return nil
	}

	previous := c.session.Snapshot()
	if _, err := c.session.CommitQuery(spec.Query, generation); err != nil {
		return err
	}
	if previous.Query == nil {
		c.queryMarks.mark(c.snapshotConnections())
		return nil
	}
	c.appendReplacedResult(previous)
	return nil
}

// appendReplacedResult appends the result of previous, a query a reload just
// replaced, if it is a cumulative "outfile append" query: its interim
// reports wrote nothing (writeCumulativeResult), so without this what it
// received would never reach the outfile. As for dropQuery, the servers
// aborted its aggregate, so this is what arrived until the reload. The
// snapshot is stale by now (CommitQuery replaced it), so it is written
// directly; later aggregates go to the new generation's group set (a merge
// that took the old snapshot just before the commit may still finish after
// this write, its rows are then lost like the aborted ones). Other queries
// are left alone: their interim reports already wrote their results.
func (c *MaprClient) appendReplacedResult(previous maprclient.SessionSnapshot) {
	if previous.GlobalGroup == nil || !previous.Query.HasOutfile() ||
		!previous.Query.Outfile.AppendMode || !c.isCumulative(previous.Query) {
		return
	}
	if err := c.writeCumulativeResult(previous, true); err != nil {
		// On the control loop: never the client logger (controlLogger).
		c.controlLogger().Error("Unable to append the replaced query's mapreduce result", err)
	}
}

// dropQuery ends the active query, if any, of a switchable client whose
// session switched back to following lines at generation. The servers
// already stopped the query (they acknowledged the update), and they abort
// its aggregate when the generation is replaced instead of sending it once
// more: what they aggregated since their last periodic serialization (up to
// one interval of the query) never reaches the client. What did arrive is
// reported once more, as the query's last result, as finish does
// (reportResults(true)): with an outfile it is always written; without one
// printResults prints it only if the rendered table has rows and changed
// since the last print, and for interactive dtail without an outfile (not
// cumulative) it covers only what arrived since the last report. Then the
// client-side state is cleared (SessionState.ClearQuery), which also ends
// the periodic reports until a new query is committed.
func (c *MaprClient) dropQuery(generation uint64) {
	if c.session.Snapshot().Query != nil {
		// On the control loop: printed and logged without waiting for a
		// paused client logger (controlLogger).
		out := c.controlLogger()
		if err := c.reportResultsTo(out, true); err != nil {
			out.Error("Unable to write final mapreduce result", err)
		}
	}
	c.session.ClearQuery(generation)
}

// isCumulative reports whether the results of query add up over the
// intervals. The args are read under the state lock: an interactive reload
// or workload switch replaces them while the reporter runs.
func (c *MaprClient) isCumulative(query *mapr.Query) bool {
	switch c.mode {
	case CumulativeMode, ScheduledMode, ScheduledPartialMode:
		return true
	case NonCumulativeMode:
		return false
	default:
		args, _ := c.snapshotConnectionState()
		return args.Mode == omode.MapClient || (query != nil && query.HasOutfile())
	}
}

// warnUnknownQueryVariables writes a plan-time warning to w for each
// $-variable the query references that the selected parser cannot populate.
// The client cannot know a remote server's configured MapreduceLogFormat, so it
// assumes the DTail default ("default") for the from-TABLE case; this matches
// the stock server configuration. Unknown/custom server formats therefore do
// not get this diagnostic (see PlanVariableWarnings, which is a no-op for
// non-enumerable formats).
func warnUnknownQueryVariables(w io.Writer, query *mapr.Query, logger logging.Logger) {
	logFormat := query.EffectiveLogFormat("")
	for _, warning := range logformat.PlanVariableWarnings(query, logFormat) {
		if _, err := fmt.Fprintln(w, warning); err != nil {
			logger.Debug("Unable to write query variable warning", err)
		}
	}
}

// reportDelay is the time until the next interim report: the query's
// interval (mapr.NewQuery defaults it to 5 s), which also paces each
// dserver's periodic serialization (aggregate.Start), or half of it for the
// first report of a query generation (rampUp; reportGeneration moves that
// report to shortly after the generation's first aggregates). The 1 s
// fallback only applies to "interval 0" (and a missing query), matching the
// server's fallback.
func (c *MaprClient) reportDelay(query *mapr.Query, rampUp bool) time.Duration {
	interval := time.Second
	if query != nil && query.Interval > 0 {
		interval = query.Interval
	}
	if !rampUp {
		return interval
	}

	delay := interval / 2
	if delay <= 0 {
		return interval
	}
	return delay
}
