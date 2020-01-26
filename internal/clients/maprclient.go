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
	additative bool
}

// NewMaprClient returns a new mapreduce client.
func NewMaprClient(args Args, queryStr string) (*MaprClient, error) {
	if queryStr == "" {
		return nil, errors.New("No mapreduce query specified, use '-query' flag")
	}

	c := MaprClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      args.Mode == omode.TailClient,
		},
		queryStr:   queryStr,
		additative: args.Mode == omode.MapClient,
	}

	query, err := mapr.NewQuery(c.queryStr)
	if err != nil {
		logger.FatalExit(c.queryStr, "Can't parse mapr query", err)
	}

	c.query = query

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
	if c.additative {
		c.recievedFinalResult()
	}

	return
}

func (c MaprClient) makeHandler(server string) handlers.Handler {
	return handlers.NewMaprHandler(server, c.query, c.globalGroup)
}

func (c MaprClient) makeCommands() (commands []string) {
	commands = append(commands, fmt.Sprintf("map %s", c.query.RawQuery))

	modeStr := "tail"
	if c.additative {
		modeStr = "cat"
	}

	for _, file := range strings.Split(c.What, ",") {
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

	if c.additative {
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
