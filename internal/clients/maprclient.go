package clients

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/omode"
)

// MaprClient is used for running mapreduce aggregations on remote files.
type MaprClient struct {
	baseClient
	// Query string for mapr aggregations
	queryStr string
	// Global group set for merged mapr aggregation results
	globalGroup *mapr.GlobalGroupSet
	// The query object (constructed from queryStr)
	query *mapr.Query
	// Additative result or new result every run?
	cumulative bool
}

// NewMaprClient returns a new mapreduce client.
func NewMaprClient(args Args, queryStr string) (*MaprClient, error) {
	if queryStr == "" {
		return nil, errors.New("No mapreduce query specified, use '-query' flag")
	}

	query, err := mapr.NewQuery(queryStr)
	if err != nil {
		logger.FatalExit(queryStr, "Can't parse mapr query", err)
	}

	// Don't retry connection if in tail mode and no outfile specified.
	retry := args.Mode == omode.TailClient && query.Outfile == ""

	// Result is comulative if we are in MapClient mode or with outfile
	cumulative := args.Mode == omode.MapClient || query.Outfile != ""

	c := MaprClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      retry,
		},
		query:      query,
		queryStr:   queryStr,
		cumulative: cumulative,
	}

	switch c.query.Table {
	case "*":
		c.Regex = fmt.Sprintf("\\|MAPREDUCE:\\|")
	case ".":
		c.Regex = "."
	default:
		c.Regex = fmt.Sprintf("\\|MAPREDUCE:%s\\|", c.query.Table)
	}

	c.globalGroup = mapr.NewGlobalGroupSet()
	c.baseClient.init(c)

	return &c, nil
}

// Start starts the mapreduce client.
func (c *MaprClient) Start(ctx context.Context) (status int) {
	if c.query.Outfile == "" {
		// Only print out periodic results if we don't write an outfile
		go c.periodicPrintResults(ctx)
	}

	status = c.baseClient.Start(ctx)
	if c.cumulative {
		c.recievedFinalResult()
	}

	return
}

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
		if c.Timeout > 0 {
			commands = append(commands, fmt.Sprintf("timeout %d %s %s regex %s", c.Timeout, modeStr, file, c.Regex))
			continue
		}
		commands = append(commands, fmt.Sprintf("%s %s regex %s", modeStr, file, c.Regex))
	}

	return
}

func (c *MaprClient) recievedFinalResult() {
	logger.Info("Received final mapreduce result")

	if c.query.Outfile == "" {
		c.printResults()
		return
	}

	logger.Info(fmt.Sprintf("Writing final mapreduce result to '%s'", c.query.Outfile))
	err := c.globalGroup.WriteResult(c.query)
	if err != nil {
		logger.FatalExit(err)
		return
	}
	logger.Info(fmt.Sprintf("Wrote final mapreduce result to '%s'", c.query.Outfile))
}

func (c *MaprClient) periodicPrintResults(ctx context.Context) {
	for {
		select {
		case <-time.After(c.query.Interval):
			logger.Info("Gathering interim mapreduce result")
			c.printResults()
		case <-ctx.Done():
			return
		}
	}
}

func (c *MaprClient) printResults() {
	var result string
	var err error
	var numLines int

	if c.cumulative {
		result, numLines, err = c.globalGroup.Result(c.query)
	} else {
		result, numLines, err = c.globalGroup.SwapOut().Result(c.query)
	}
	if err != nil {
		logger.FatalExit(err)
	}
	if numLines > 0 {
		logger.Raw(fmt.Sprintf("%s\n", c.query.RawQuery))
		logger.Raw(result)
	}
}
