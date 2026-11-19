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
	// Selected cumulative reporting mode.
	mode MaprClientMode
	// Publication state prevents an empty interval from replacing a result
	// produced earlier in the same query generation.
	outfileState outfileReportState
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
	loggers = loggers.normalized()

	query, err := mapr.NewQuery(args.QueryStr, loggers.Client)
	if err != nil {
		return nil, fmt.Errorf("parse mapreduce query %q: %w", args.QueryStr, err)
	}

	// Warn once, at plan time, about $-variables the selected parser cannot
	// populate. This runs in the user's client process for both server and
	// serverless mode, so the warning reaches the user's stderr even though the
	// actual field resolution happens server-side.
	warnUnknownQueryVariables(os.Stderr, query, loggers.Client)
	warnIgnoredInvert(os.Stderr, args, loggers.Client)

	// Don't retry connection if in tail mode and no outfile specified.
	retry := args.Mode == omode.TailClient && !query.HasOutfile()

	c := MaprClient{
		session: maprclient.NewSessionState(query, loggers.Client),
		mode:    maprClientMode,
	}

	applyQueryRegex(&args, query)
	base, initErr := newBaseClient(args, cfg, loggers, firstColorizer(colorizers), clientProfile{
		newHandler: func(server string) handlers.Handler {
			return handlers.NewMaprHandler(server, c.session, loggers.Client)
		},
		retry:  retry,
		commit: c.commitSessionSpec,
	})
	if initErr != nil {
		return nil, fmt.Errorf("initialize mapreduce client: %w", initErr)
	}
	c.baseClient = base
	loggers.Client.Debug("Cumulative mapreduce mode?", c.isCumulative(query))

	return &c, nil
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
	if c.mode.scheduled() {
		return c.finishScheduled(ctxErr, status)
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
		if !conn.Handler().HasCapability(protocol.CapabilityCommandFailureV1) {
			c.clientLogger().Debug(conn.Server(), "Server does not report failed commands, "+
				"only an incomplete session fails the query", protocol.CapabilityCommandFailureV1)
		}
	}
	return outcome
}

// sessionOutcome returns why the session of handler with server did not
// complete, or "" if it did, and the file read failures (see
// protocol.IsFileReadFailure) the server reported, each as "<server>:
// <reason>". The session completed when the server ended it with its close
// handshake (so it was not cut short, e.g. by a killed or shut down server or
// a lost connection) and reported no failed command other than file read
// failures. A server not advertising protocol.CapabilityCommandFailureV1
// reports no failed commands, so for it only the close handshake counts.
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
// how its session ended is left out too.
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
		for _, reason := range outcome.Failures {
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

func (c *MaprClient) periodicReportResults(ctx context.Context) {
	var (
		lastGeneration uint64
		seenGeneration bool
	)

	for {
		if ctx.Err() != nil {
			return
		}
		snapshot := c.session.Snapshot()
		rampUp := !seenGeneration || snapshot.Generation != lastGeneration
		lastGeneration = snapshot.Generation
		seenGeneration = true

		delay := c.reportDelay(snapshot.Query, rampUp)
		c.clientLogger().Debug("Sleeping before processing mapreduce results", "generation", snapshot.Generation, "delay", delay)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			c.clientLogger().Debug("Gathering interim mapreduce result")
			if err := c.reportResults(false); err != nil {
				c.clientLogger().Error("Unable to gather mapreduce result", err)
			}
		case <-c.session.Changes():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			c.clientLogger().Debug("Mapreduce query generation changed, recalculating report interval")
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (c *MaprClient) reportResults(finalResult bool) error {
	snapshot := c.session.Snapshot()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		return nil
	}

	if snapshot.Query.HasOutfile() {
		return c.writeResultsToOutfile(snapshot, finalResult)
	}
	return c.printResults(snapshot)
}

func (c *MaprClient) printResults(snapshot maprclient.SessionSnapshot) error {
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
		c.clientLogger().Debug("Discarding stale mapreduce result", "generation", snapshot.Generation)
		return nil
	}
	if !changed {
		c.clientLogger().Debug("Result hasn't changed compared to last time...")
		return nil
	}

	if numRows == 0 {
		c.clientLogger().Debug("Empty result set this time...")
		return nil
	}

	rawQuery := c.runtime.output.PaintMaprRawQuery(snapshot.Query.RawQuery)
	clientlog.Raw(c.clientLogger(), fmt.Sprintf("%s\n", rawQuery))

	if rowsLimit > 0 && numRows > rowsLimit {
		c.clientLogger().Warn(fmt.Sprintf("Got %d results but limited terminal output "+
			"to %d rows! Use 'limit' clause to override!", numRows, rowsLimit))
	}
	clientlog.Raw(c.clientLogger(), fmt.Sprintf("%s\n", result))
	return nil
}

func (c *MaprClient) writeResultsToOutfile(snapshot maprclient.SessionSnapshot, finalResult bool) error {
	current, err := c.session.WithCurrentSnapshot(snapshot, func() error {
		// Lock order inside the callback is SessionState read lock, then
		// outfileState.mu, then the aggregate/file locks. Do not re-enter
		// SessionState from this callback.
		return c.writeCurrentResultsToOutfile(snapshot, finalResult)
	})
	if !current {
		c.clientLogger().Debug("Discarding stale mapreduce outfile result", "generation", snapshot.Generation)
		return nil
	}
	return err
}

func (c *MaprClient) writeCurrentResultsToOutfile(snapshot maprclient.SessionSnapshot, finalResult bool) error {
	cumulative := c.isCumulative(snapshot.Query)
	c.clientLogger().Debug("writeResultsToOutfile called", "finalResult", finalResult, "cumulative", cumulative, "generation", snapshot.Generation)
	if cumulative {
		if err := snapshot.GlobalGroup.WriteResult(snapshot.Query, finalResult); err != nil {
			return fmt.Errorf("unable to write cumulative mapreduce result: %w", err)
		}
		c.clientLogger().Debug("WriteResult completed for cumulative mode")
		return nil
	}

	if snapshot.Query.Outfile.AppendMode {
		group := snapshot.GlobalGroup.SwapOut()
		if err := group.WriteResult(snapshot.Query, true); err != nil {
			return fmt.Errorf("unable to write non-cumulative mapreduce result: %w", err)
		}
		c.clientLogger().Debug("WriteResult completed for non-cumulative append mode")
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
		c.clientLogger().Debug("Preserving previous non-empty outfile result", "generation", snapshot.Generation)
		return nil
	}
	if err := group.WriteResult(snapshot.Query, true); err != nil {
		return fmt.Errorf("unable to write non-cumulative mapreduce result: %w", err)
	}
	if !empty {
		c.outfileState.hasRows = true
	}
	c.clientLogger().Debug("WriteResult completed for non-cumulative mode")
	return nil
}

// commitSessionSpec is the profile commit of an interactive reload: it
// switches the client-side aggregation to the reloaded query at the new
// generation. It does not touch c.Args: reconnects read them concurrently
// under the state lock, and applyInteractiveReload stores the reloaded args
// (with the query and its normalized line filter, see
// normalizeInteractiveArgs) under that lock right after this commit.
func (c *MaprClient) commitSessionSpec(spec SessionSpec, generation uint64) error {
	if spec.Query == "" {
		return errors.New("missing mapreduce query")
	}

	_, err := c.session.CommitQuery(spec.Query, generation)
	return err
}

func (c *MaprClient) isCumulative(query *mapr.Query) bool {
	switch c.mode {
	case CumulativeMode, ScheduledMode, ScheduledPartialMode:
		return true
	case NonCumulativeMode:
		return false
	default:
		return c.Mode == omode.MapClient || (query != nil && query.HasOutfile())
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
