package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

type mapParserFactory func(string, *mapr.Query, string) (logformat.Parser, error)

// Map command implements the mapreduce command server side.
type mapCommand struct {
	aggregate *mapaggregate.Aggregate
	server    *ServerHandler
}

// newMapCommand returns a new server side mapreduce command.
//
// The Aggregate is the one and only aggregate path for BOTH server mode and
// serverless: an Aggregate is always built, and the read commands feed it
// directly via Processor (no aggregate line-channel). The former
// regular aggregate implementation has been deleted (task hv0), so there is no
// fallback branch.
func newMapCommand(serverHandler *ServerHandler, args []string) (mapCommand, *mapaggregate.Aggregate, error) {

	m := mapCommand{server: serverHandler}
	queryStr := strings.Join(args[1:], " ")
	logger := serverHandler.Logger()
	query, err := mapr.NewQuery(queryStr, logger)
	if err != nil {
		return m, nil, fmt.Errorf("parse map query: %w", err)
	}
	defaultLogFormat := ""
	if serverHandler.serverCfg != nil {
		defaultLogFormat = serverHandler.serverCfg.MapreduceLogFormat
	}

	parser, err := resolveMapParser(query, defaultLogFormat, serverHandler.hostname,
		logger, logformat.NewParserWithHostname)
	if err != nil {
		return m, nil, err
	}

	logger.Info("Creating turbo aggregate for MapReduce", "query", queryStr)
	aggregate, err := mapaggregate.New(query, parser, serverHandler.hostname, logger)
	if err != nil {
		return m, nil, err
	}
	m.aggregate = aggregate
	return m, aggregate, nil
}

func resolveMapParser(query *mapr.Query, defaultLogFormat, hostname string,
	logger logging.Logger, factory mapParserFactory) (logformat.Parser, error) {
	parserName := query.EffectiveLogFormat(defaultLogFormat)
	logger.Info("Creating log format parser",
		"parserName", parserName,
		"queryTable", query.Table,
		"queryLogFormat", query.LogFormat)
	parser, err := factory(parserName, query, hostname)
	if err == nil {
		return parser, nil
	}

	logger.Error("Could not create log format parser. Falling back to 'generic'", err)
	parser, fallbackErr := factory("generic", query, hostname)
	if fallbackErr != nil {
		return nil, fmt.Errorf("create fallback generic log format parser: %w", fallbackErr)
	}
	return parser, nil
}

func (m *mapCommand) Start(ctx context.Context, aggregatedMessages chan<- string) {
	m.aggregate.Start(ctx, aggregatedMessages)
}
