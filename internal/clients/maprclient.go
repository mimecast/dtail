package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

	// Don't retry connection if in tail mode and no outfile specified.
	retry := args.Mode == omode.TailClient && !query.HasOutfile()

	c := MaprClient{
		session: maprclient.NewSessionState(query, loggers.Client),
		mode:    maprClientMode,
	}

	args.RegexStr = maprRegexForQuery(query)
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
func (c *MaprClient) Start(ctx context.Context, statsCh <-chan string) (status int) {
	go c.periodicReportResults(ctx)

	status = c.baseClient.Start(ctx, statsCh)

	// Always write final result for cumulative mode (includes outfile case)
	if snapshot := c.session.Snapshot(); c.isCumulative(snapshot.Query) {
		c.clientLogger().Debug("Writing final mapreduce result")
		if err := c.reportResults(true); err != nil {
			c.clientLogger().Error("Unable to write final mapreduce result", err)
		}
		c.clientLogger().Debug("Final result written")
	}

	return
}

func (c *MaprClient) periodicReportResults(ctx context.Context) {
	var (
		lastGeneration uint64
		seenGeneration bool
	)

	for {
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

func (c *MaprClient) commitSessionSpec(spec SessionSpec, generation uint64) error {
	if spec.Query == "" {
		return errors.New("missing mapreduce query")
	}

	query, err := c.session.CommitQuery(spec.Query, generation)
	if err != nil {
		return err
	}

	c.QueryStr = spec.Query
	c.setRegexForQuery(query)
	return nil
}

func (c *MaprClient) isCumulative(query *mapr.Query) bool {
	switch c.mode {
	case CumulativeMode:
		return true
	case NonCumulativeMode:
		return false
	default:
		return c.Mode == omode.MapClient || (query != nil && query.HasOutfile())
	}
}

func (c *MaprClient) setRegexForQuery(query *mapr.Query) {
	c.RegexStr = maprRegexForQuery(query)
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
