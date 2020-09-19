package server

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

// Aggregate is for aggregating mapreduce data on the DTail server side.
type Aggregate struct {
	done *internal.Done
	// Log lines to process (parsing MAPREDUCE lines).
	Lines chan line.Line
	// Hostname of the current server (used to populate $hostname field).
	hostname string
	// Signals to serialize data.
	serialize chan struct{}
	// Signals to flush data.
	flush chan struct{}
	// Signals that data has been flushed
	flushed chan struct{}
	// The mapr query
	query *mapr.Query
	// The mapr log format parser
	parser *logformat.Parser
}

// NewAggregate return a new server side aggregator.
func NewAggregate(queryStr string) (*Aggregate, error) {
	query, err := mapr.NewQuery(queryStr)
	if err != nil {
		return nil, err
	}

	fqdn, err := os.Hostname()
	if err != nil {
		logger.Error(err)
	}
	s := strings.Split(fqdn, ".")

	var parserName string
	switch query.LogFormat {
	case "":
		parserName = config.Server.MapreduceLogFormat
		if query.Table == "" {
			parserName = "generic"
		}
	default:
		parserName = query.LogFormat
	}

	logger.Info("Creating log format parser", parserName)
	logParser, err := logformat.NewParser(parserName, query)
	if err != nil {
		logger.Error("Could not create log format parser. Falling back to 'generic'", err)
		if logParser, err = logformat.NewParser("generic", query); err != nil {
			logger.FatalExit("Could not create log format parser", err)
		}
	}

	a := Aggregate{
		done:      internal.NewDone(),
		Lines:     make(chan line.Line, 100),
		serialize: make(chan struct{}),
		flush:     make(chan struct{}),
		flushed:   make(chan struct{}),
		hostname:  s[0],
		query:     query,
		parser:    logParser,
	}

	return &a, nil
}

func (a *Aggregate) Shutdown() {
	a.Flush()
	a.done.Shutdown()
}

// Start an aggregation.
func (a *Aggregate) Start(ctx context.Context, maprLines chan<- string) {

	myCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-myCtx.Done():
			a.done.Shutdown()
		case <-a.done.Done():
			cancel()
		}
	}()

	fieldsCh := a.makeFields(myCtx)

	// Add fields (e.g. via 'set' clause)
	if len(a.query.Set) > 0 {
		fieldsCh = a.addFields(myCtx, fieldsCh)
	}

	go a.aggregateTimer(myCtx)
	a.makeMaprLines(myCtx, fieldsCh, maprLines)
}

func (a *Aggregate) aggregateTimer(ctx context.Context) {
	for {
		select {
		case <-time.After(a.query.Interval):
			a.Serialize(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Aggregate) makeFields(ctx context.Context) <-chan map[string]string {
	ch := make(chan map[string]string)

	go func() {
		defer close(ch)

		for {
			select {
			case line, ok := <-a.Lines:
				if !ok {
					return
				}

				maprLine := strings.TrimSpace(string(line.Content))
				fields, err := a.parser.MakeFields(maprLine)
				logger.Debug(fields, err)

				if err != nil {
					logger.Error(err)
					continue
				}
				if !a.query.WhereClause(fields) {
					continue
				}

				select {
				case ch <- fields:
				case <-ctx.Done():
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch
}

func (a *Aggregate) addFields(ctx context.Context, fieldsCh <-chan map[string]string) <-chan map[string]string {
	ch := make(chan map[string]string)

	go func() {
		defer close(ch)

		for {
			// fieldsCh will be closed via 'makeFields' if ctx is done
			fields, ok := <-fieldsCh
			if !ok {
				return
			}
			if err := a.query.SetClause(fields); err != nil {
				logger.Error(err)
			}

			select {
			case ch <- fields:
			case <-ctx.Done():
			}
		}
	}()

	return ch
}

func (a *Aggregate) makeMaprLines(ctx context.Context, fieldsCh <-chan map[string]string, maprLines chan<- string) {
	group := mapr.NewGroupSet()

	serialize := func() {
		logger.Info("Serializing mapreduce result")
		group.Serialize(ctx, maprLines)
		group = mapr.NewGroupSet()
		logger.Info("Done serializing mapreduce result")
	}

	for {
		select {
		case fields, ok := <-fieldsCh:
			if !ok {
				serialize()
				return
			}
			a.aggregate(group, fields)
		case <-a.serialize:
			serialize()
		case <-a.flush:
			serialize()
			a.flushed <- struct{}{}
		case <-ctx.Done():
			return
		}
	}
}

func (a *Aggregate) aggregate(group *mapr.GroupSet, fields map[string]string) {
	//logger.Trace("Aggregating", group, fields)
	var sb strings.Builder

	for i, field := range a.query.GroupBy {
		if i > 0 {
			sb.WriteString(" ")
		}
		if val, ok := fields[field]; ok {
			sb.WriteString(val)
		}
	}
	groupKey := sb.String()
	set := group.GetSet(groupKey)

	var addedSample bool
	for _, sc := range a.query.Select {
		if val, ok := fields[sc.Field]; ok {
			if err := set.Aggregate(sc.FieldStorage, sc.Operation, val, false); err != nil {
				logger.Error(err)
				continue
			}
			addedSample = true
		}
	}

	if addedSample {
		set.Samples++
		return
	}

	logger.Trace("Aggregated data locally without adding new samples")
}

// Serialize all the aggregated data.
func (a *Aggregate) Serialize(ctx context.Context) {
	select {
	case a.serialize <- struct{}{}:
	case <-time.After(time.Minute):
		logger.Warn("Starting to serialize mapredice data takes over a minute")
	case <-ctx.Done():
	}
}

// Flush all data.
func (a *Aggregate) Flush() {
	select {
	case a.flush <- struct{}{}:
		logger.Info("Flushing mapreduce data")
	case <-time.After(time.Minute):
		logger.Warn("Starting to flush mapreduce data takes over a minute")
		return
	case <-a.done.Done():
		return
	}

	select {
	case <-a.flushed:
		logger.Info("Done flushing")
	case <-time.After(time.Minute):
		logger.Warn("Waiting for data to be flushed takes over a minute")
	case <-a.done.Done():
	}
}
