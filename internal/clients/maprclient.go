package clients

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/mapr"
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
	// Global group set for merged mapr aggregation results
	globalGroup *mapr.GlobalGroupSet
	// The query object (constructed from queryStr)
	query *mapr.Query
	// Additative result or new result every interval run?
	cumulative bool
	// The last result string received
	lastResult string
}

// NewMaprClient returns a new mapreduce client.
func NewMaprClient(args config.Args, maprClientMode MaprClientMode) (*MaprClient, error) {
	if args.QueryStr == "" {
		return nil, errors.New("No mapreduce query specified, use '-query' flag")
	}

	query, err := mapr.NewQuery(args.QueryStr)
	if err != nil {
		dlog.Client.FatalPanic(args.QueryStr, "Can't parse mapr query", err)
	}

	// Don't retry connection if in tail mode and no outfile specified.
	retry := args.Mode == omode.TailClient && !query.HasOutfile()

	var cumulative bool
	switch maprClientMode {
	case CumulativeMode:
		cumulative = true
	case NonCumulativeMode:
		cumulative = false
	default:
		// Result is comulative if we are in MapClient mode or with outfile
		cumulative = args.Mode == omode.MapClient || query.HasOutfile()
	}

	dlog.Client.Debug("Cumulative mapreduce mode?", cumulative)

	c := MaprClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      retry,
		},
		query:      query,
		cumulative: cumulative,
	}

	switch c.query.Table {
	case "", ".":
		c.RegexStr = "."
	case "*":
		c.RegexStr = fmt.Sprintf("\\|MAPREDUCE:\\|")
	default:
		c.RegexStr = fmt.Sprintf("\\|MAPREDUCE:%s\\|", c.query.Table)
	}

	c.globalGroup = mapr.NewGlobalGroupSet()
	c.baseClient.init()
	c.baseClient.makeConnections(c)

	return &c, nil
}

// Start starts the mapreduce client.
func (c *MaprClient) Start(ctx context.Context, statsCh <-chan string) (status int) {
	go c.periodicReportResults(ctx)

	status = c.baseClient.Start(ctx, statsCh)
	if c.cumulative {
		dlog.Client.Debug("Received final mapreduce result")
		c.reportResults()
	}

	return
}

// NEXT: Make this a callback function rather trying to use polymorphism to call this.
// This applies to all clients.
func (c MaprClient) makeHandler(server string) handlers.Handler {
	return handlers.NewMaprHandler(server, c.query, c.globalGroup)
}

func (c MaprClient) makeCommands() (commands []string) {
	commands = append(commands, fmt.Sprintf("map %s", c.query.RawQuery))

	modeStr := "cat"
	if c.Mode == omode.TailClient {
		modeStr = "tail"
	}

	for _, file := range strings.Split(c.What, ",") {
		regex, err := c.Regex.Serialize()
		if err != nil {
			dlog.Client.FatalPanic(err)
		}
		if c.Timeout > 0 {
			commands = append(commands, fmt.Sprintf("timeout %d %s %s %s", c.Timeout,
				modeStr, file, regex))
			continue
		}
		commands = append(commands, fmt.Sprintf("%s:%s %s %s",
			modeStr, c.Args.SerializeOptions(), file, regex))
	}

	return
}

func (c *MaprClient) periodicReportResults(ctx context.Context) {
	for {
		select {
		case <-time.After(c.query.Interval):
			dlog.Client.Debug("Gathering interim mapreduce result")
			c.reportResults()
		case <-ctx.Done():
			return
		}
	}
}

func (c *MaprClient) reportResults() {
	if c.query.HasOutfile() {
		c.writeResultsToOutfile()
		return
	}

	c.printResults()
}

func (c *MaprClient) printResults() {
	var result string
	var err error
	var numRows int
	rowsLimit := -1

	if c.query.Limit == -1 {
		// Limit output to 10 rows when the result is printed to stdout.
		// This can be overriden with the limit clause though.
		rowsLimit = 10
	}

	if c.cumulative {
		result, numRows, err = c.globalGroup.Result(c.query, rowsLimit)
	} else {
		result, numRows, err = c.globalGroup.SwapOut().Result(c.query, rowsLimit)
	}

	if err != nil {
		dlog.Client.FatalPanic(err)
	}

	if result == c.lastResult {
		dlog.Client.Debug("Result hasn't changed compared to last time...")
		return
	}
	c.lastResult = result

	if numRows == 0 {
		dlog.Client.Debug("Empty result set this time...")
		return
	}

	rawQuery := c.query.RawQuery
	if config.Client.TermColorsEnable {
		rawQuery = color.PaintStrWithAttr(rawQuery,
			config.Client.TermColors.MaprTable.RawQueryFg,
			config.Client.TermColors.MaprTable.RawQueryBg,
			config.Client.TermColors.MaprTable.RawQueryAttr)
	}
	dlog.Client.Raw(rawQuery)

	if rowsLimit > 0 && numRows > rowsLimit {
		dlog.Client.Warn(fmt.Sprintf("Got %d results but limited terminal output to %d rows! Use 'limit' clause to override!",
			numRows, rowsLimit))
	}
	dlog.Client.Raw(result)
}

func (c *MaprClient) writeResultsToOutfile() {
	if c.cumulative {
		if err := c.globalGroup.WriteResult(c.query); err != nil {
			dlog.Client.FatalPanic(err)
		}
		return
	}

	if err := c.globalGroup.SwapOut().WriteResult(c.query); err != nil {
		dlog.Client.FatalPanic(err)
	}
}
