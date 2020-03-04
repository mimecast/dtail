package server

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

// Aggregate is for aggregating mapreduce data on the DTail server side.
type Aggregate struct {
	// Log lines to process (parsing MAPREDUCE lines).
	Lines chan line.Line
	// Hostname of the current server (used to populate $hostname field).
	hostname string
	// Signals to serialize data.
	serialize chan struct{}
	// Signals to flush data.
	flush chan struct{}
	// The mapr query
	query *mapr.Query
	// The mapr log format parser
	parser *logformat.Parser
	cancel context.CancelFunc
	ctx    context.Context
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

	logger.Info("Creating mapr log format parser", config.Server.MapreduceLogFormat)
	logParser, err := logformat.NewParser(config.Server.MapreduceLogFormat)
	if err != nil {
		logger.FatalExit("Could not create mapr log format parser", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	a := Aggregate{
		Lines:     make(chan line.Line, 100),
		serialize: make(chan struct{}),
		flush:     make(chan struct{}),
		hostname:  s[0],
		query:     query,
		parser:    logParser,
		ctx:       ctx,
		cancel:    cancel,
	}

	return &a, nil
}

// Start an aggregation.
func (a *Aggregate) Start(ctx context.Context, maprLines chan<- string) {
	defer a.cancel()

	fieldsCh := a.linesToFields(ctx)
	go a.fieldsToMaprLines(ctx, fieldsCh, maprLines)
	a.periodicAggregateTimer(ctx)
}

// Cancel the aggregation.
func (a *Aggregate) Cancel() {
	a.cancel()
}

func (a *Aggregate) periodicAggregateTimer(ctx context.Context) {
	for {
		select {
		case <-time.After(a.query.Interval):
			a.Serialize(ctx)
		case <-ctx.Done():
			return
		case <-a.ctx.Done():
			return
		}
	}
}

func (a *Aggregate) linesToFields(ctx context.Context) <-chan map[string]string {
	fieldsCh := make(chan map[string]string)

	go func() {
		defer close(fieldsCh)

		for {
			select {
			case line, ok := <-a.Lines:
				if !ok {
					return
				}

				maprLine := strings.TrimSpace(string(line.Content))
				fields, err := a.parser.MakeFields(maprLine)

				if err != nil {
					logger.Error(err)
					continue
				}
				if !a.query.WhereClause(fields) {
					continue
				}

				select {
				case fieldsCh <- fields:
				case <-ctx.Done():
				}
			case <-ctx.Done():
				return
			case <-a.ctx.Done():
				return
			}
		}
	}()

	return fieldsCh
}

func (a *Aggregate) fieldsToMaprLines(ctx context.Context, fieldsCh <-chan map[string]string, maprLines chan<- string) {
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
			logger.Info("Flushing mapreduce result")
			serialize()
			a.flush <- struct{}{}
			logger.Info("Done flushing mapreduce result")
		case <-ctx.Done():
			return
		case <-a.ctx.Done():
			logger.Info("Flushing mapreduce result")
			serialize()
			a.flush <- struct{}{}
			logger.Info("Done flushing mapreduce result")
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
	case <-ctx.Done():
	}
}

// Flush all data.
func (a *Aggregate) Flush() {
	select {
	case <-a.ctx.Done():
		return
	case a.flush <- struct{}{}:
	case <-time.After(time.Minute):
		return
	}

	select {
	case <-a.flush:
	case <-time.After(time.Minute):
	}
}
