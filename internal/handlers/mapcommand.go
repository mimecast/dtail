package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

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

	factory := serverHandler.mapParserFactory
	if factory == nil {
		factory = logformat.NewParserWithHostname
	}
	parser, err := resolveMapParser(query, defaultLogFormat, serverHandler.hostname,
		logger, factory)
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

// preparedMapCommand is the map command of a SESSION START/UPDATE, built
// before the request changes the session (see prepareSessionMapCommand), so
// that a map command that can not be built rejects the request while the
// running generation keeps running. handleMapCommand takes it from the
// dispatch context instead of building the command again.
type preparedMapCommand struct {
	taken   atomic.Bool
	command mapCommand
}

type preparedMapCommandKeyType struct{}

var preparedMapCommandKey preparedMapCommandKeyType

// prepareSessionMapCommand builds the map command among commands, the
// commands of a SESSION START/UPDATE spec (see session.Spec.Commands), and
// returns ctx carrying it for dispatchSessionCommands. A spec without a query
// has no map command, and ctx is returned as it is.
func (h *ServerHandler) prepareSessionMapCommand(ctx context.Context, commands []string) (context.Context, error) {
	for _, command := range commands {
		args := strings.Split(command, " ")
		if name, _, _ := strings.Cut(args[0], ":"); name != "map" {
			continue
		}
		prepared, _, err := newMapCommand(h, args)
		if err != nil {
			return ctx, err
		}
		holder := &preparedMapCommand{command: prepared}
		return context.WithValue(ctx, preparedMapCommandKey, holder), nil
	}
	return ctx, nil
}

// mapCommandFor returns the map command prepared for ctx's SESSION
// START/UPDATE, once, or else builds it from args.
func (h *ServerHandler) mapCommandFor(ctx context.Context, args []string) (mapCommand, *mapaggregate.Aggregate, error) {
	if holder, ok := ctx.Value(preparedMapCommandKey).(*preparedMapCommand); ok &&
		holder.taken.CompareAndSwap(false, true) {
		return holder.command, holder.command.aggregate, nil
	}
	return newMapCommand(h, args)
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
