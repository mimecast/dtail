package server

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// Aggregate is for aggregating mapreduce data on the DTail server side.
type Aggregate struct {
	done *internal.Done
	// NextLinesCh can be used to use a new line ch.
	NextLinesCh chan chan line.Line
	// Hostname of the current server (used to populate $hostname field).
	hostname string
	// Signals to serialize data.
	serialize chan struct{}
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
		dlog.Common.Error(err)
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

	dlog.Common.Info("Creating log format parser", parserName)
	logParser, err := logformat.NewParser(parserName, query)
	if err != nil {
		dlog.Common.Error("Could not create log format parser. Falling back to 'generic'", err)
		if logParser, err = logformat.NewParser("generic", query); err != nil {
			dlog.Common.FatalPanic("Could not create log format parser", err)
		}
	}

	a := Aggregate{
		done:        internal.NewDone(),
		NextLinesCh: make(chan chan line.Line, 10),
		serialize:   make(chan struct{}),
		hostname:    s[0],
		query:       query,
		parser:      logParser,
	}

	return &a, nil
}

// Shutdown the aggregation engine.
func (a *Aggregate) Shutdown() {
	a.done.Shutdown()
}

// Start an aggregation.
func (a *Aggregate) Start(ctx context.Context, maprMessages chan<- string) {
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

	fieldsCh := a.fieldsFromLines(myCtx)

	// Add fields (e.g. via 'set' clause)
	if len(a.query.Set) > 0 {
		fieldsCh = a.setAdditionalFields(myCtx, fieldsCh)
	}

	// Periodically pre-aggregate data every a.query.Interval seconds.
	go a.aggregateTimer(myCtx)
	a.aggregateAndSerialize(myCtx, fieldsCh, maprMessages)
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

func (a *Aggregate) fieldsFromLines(ctx context.Context) <-chan map[string]string {
	fieldsCh := make(chan map[string]string)

	go func() {
		defer close(fieldsCh)
		var lines chan line.Line

		// Gather first lines channel (first input file)
		select {
		case lines = <-a.NextLinesCh:
		case <-ctx.Done():
			return
		}

		for {
			select {
			case line, ok := <-lines:
				if !ok {
					select {
					case lines = <-a.NextLinesCh:
						// Have a new lines channel (e.g. new input file)
					case <-ctx.Done():
					default:
						// No new lines channel found.
						return
					}
				}

				maprLine := strings.TrimSpace(line.Content.String())
				fields, err := a.parser.MakeFields(maprLine)
				pool.RecycleBytesBuffer(line.Content)

				if err != nil {
					// Should fields be ignored anyway?
					if err != logformat.IgnoreFieldsErr {
						dlog.Common.Error(fields, err)
					}
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
			}
		}
	}()

	return fieldsCh
}

func (a *Aggregate) setAdditionalFields(ctx context.Context, fieldsCh <-chan map[string]string) <-chan map[string]string {
	newFieldsCh := make(chan map[string]string)

	go func() {
		defer close(newFieldsCh)

		for {
			fields, ok := <-fieldsCh
			if !ok {
				return
			}
			if err := a.query.SetClause(fields); err != nil {
				dlog.Common.Error(err)
			}

			select {
			case newFieldsCh <- fields:
			case <-ctx.Done():
			}
		}
	}()

	return newFieldsCh
}

func (a *Aggregate) aggregateAndSerialize(ctx context.Context, fieldsCh <-chan map[string]string, maprMessages chan<- string) {
	group := mapr.NewGroupSet()

	serialize := func() {
		dlog.Common.Info("Serializing mapreduce result")
		group.Serialize(ctx, maprMessages)
		group = mapr.NewGroupSet()
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
		case <-ctx.Done():
			return
		}
	}
}

func (a *Aggregate) aggregate(group *mapr.GroupSet, fields map[string]string) {
	var sb strings.Builder

	for i, field := range a.query.GroupBy {
		if i > 0 {
			sb.WriteString(protocol.AggregateGroupKeyCombinator)
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
				dlog.Common.Error(err)
				continue
			}
			addedSample = true
		}
	}

	if addedSample {
		set.Samples++
		return
	}

	dlog.Common.Trace("Aggregated data locally without adding new samples")
}

// Serialize all the aggregated data.
func (a *Aggregate) Serialize(ctx context.Context) {
	select {
	case a.serialize <- struct{}{}:
	case <-time.After(time.Minute):
		dlog.Common.Warn("Starting to serialize mapredice data takes over a minute")
	case <-ctx.Done():
	}
}
