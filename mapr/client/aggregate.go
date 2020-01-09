package client

import (
	"dtail/logger"
	"dtail/mapr"
	"strconv"
	"strings"
)

// Aggregate mapreduce data on the DTail client side.
type Aggregate struct {
	// This is the mapr query specified on the command line.
	query *mapr.Query
	// This represents aggregated data of a single remote server.
	group *mapr.GroupSet
	// This represents the merged aggregated data of all servers.
	globalGroup *mapr.GlobalGroupSet
	stop        chan struct{}
	// The server we aggregate the data for (logging and debugging purposes only)
	server string
}

// NewAggregate create new client aggregator.
func NewAggregate(server string, query *mapr.Query, globalGroup *mapr.GlobalGroupSet) *Aggregate {
	return &Aggregate{
		query:       query,
		group:       mapr.NewGroupSet(),
		globalGroup: globalGroup,
		stop:        make(chan struct{}),
		server:      server,
	}
}

// Aggregate data from mapr log line into local (and global) group sets.
func (a *Aggregate) Aggregate(parts []string) {
	select {
	case <-a.stop:
		logger.Error("Client aggregator stopped for server, not processing new data", a.server)
		return
	default:
	}

	groupKey := parts[0]
	samples, err := strconv.Atoi(parts[1])
	if err != nil {
		logger.FatalExit(parts, err)
	}
	fields := a.makeFields(parts[2:])
	set := a.group.GetSet(groupKey)

	var addedSamples bool
	for _, sc := range a.query.Select {
		if val, ok := fields[sc.FieldStorage]; ok {
			if err := set.Aggregate(sc.FieldStorage, sc.Operation, val, true); err != nil {
				logger.Error(err)
				continue
			}
			addedSamples = true
		}
	}
	if addedSamples {
		set.Samples += samples
	}

	// Merge data from group into global group.
	isMerged, err := a.globalGroup.MergeNoblock(a.query, a.group)
	if err != nil {
		panic(err)
	}
	if isMerged {
		// Re-init local group (make it empty again).
		a.group.InitSet()
	}
}

// Create a map of key-value pairs from a part list such as ["foo=bar",  "bar=baz"].
func (a *Aggregate) makeFields(parts []string) map[string]string {
	fields := make(map[string]string, len(parts))

	for _, part := range parts {
		kv := strings.Split(part, "=")
		if len(kv) != 2 {
			continue
		}
		fields[kv[0]] = kv[1]
	}

	return fields
}

// Stop the client side mapreduce aggregator.
func (a *Aggregate) Stop() {
	logger.Debug("Stopping client mapreduce aggregator")
	close(a.stop)

	err := a.globalGroup.Merge(a.query, a.group)
	if err != nil {
		panic(err)
	}
}
