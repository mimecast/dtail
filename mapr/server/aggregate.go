package server

import (
	"dtail/config"
	"dtail/fs"
	"dtail/logger"
	"dtail/mapr"
	"dtail/mapr/logformat"
	"os"
	"strings"
	"time"
)

// Aggregate is for aggregating mapreduce data on the DTail server side.
type Aggregate struct {
	// Log lines to process (parsing MAPREDUCE lines).
	Lines chan fs.LineRead
	// Hostname of the current server (used to populate $hostname field).
	hostname string
	// Signals to exit goroutine.
	stop chan struct{}
	// Signals to serialize data.
	serialize chan struct{}
	// The mapr query
	query *mapr.Query
	// The mapr log format parser
	parser *logformat.Parser
}

// NewAggregate return a new server side aggregator.
func NewAggregate(maprLines chan<- string, queryStr string) (*Aggregate, error) {
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

	a := Aggregate{
		Lines:     make(chan fs.LineRead, 100),
		stop:      make(chan struct{}),
		serialize: make(chan struct{}),
		hostname:  s[0],
		query:     query,
		parser:    logParser,
	}

	go a.periodicAggregateTimer()

	fieldsCh := make(chan map[string]string)
	go a.readFields(fieldsCh, maprLines)
	go a.readLines(fieldsCh)

	return &a, nil
}

func (a *Aggregate) periodicAggregateTimer() {
	for {
		select {
		case <-time.After(a.query.Interval):
			a.Serialize()
		case <-a.stop:
			return
		}
	}
}

func (a *Aggregate) readFields(fieldsCh <-chan map[string]string, maprLines chan<- string) {
	group := mapr.NewGroupSet()

	for {
		select {
		case fields := <-fieldsCh:
			a.aggregate(group, fields)
		case <-a.serialize:
			logger.Info("Serializing mapreduce result")
			group.Serialize(maprLines, a.stop)
			logger.Info("Done serializing mapreduce result")
			group = mapr.NewGroupSet()
		case <-a.stop:
			return
		}
	}
}

func (a *Aggregate) readLines(fieldsCh chan<- map[string]string) {
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
			case <-a.stop:
			}
		case <-a.stop:
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
func (a *Aggregate) Serialize() {
	select {
	case a.serialize <- struct{}{}:
	case <-a.stop:
	}
}

// Close the aggregator.
func (a *Aggregate) Close() {
	close(a.stop)
}
