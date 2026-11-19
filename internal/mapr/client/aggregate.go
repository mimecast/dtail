package client

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

// Aggregate mapreduce data on the DTail client side.
type Aggregate struct {
	// mu protects the complete local aggregation transaction: generation
	// validation, group mutation, merge, and reset. Aggregate runs on the
	// connection's stdout-copy goroutine while Flush may run concurrently
	// during shutdown.
	mu sync.Mutex
	// This represents aggregated data of a single remote server.
	group *mapr.GroupSet
	// Shared per-client session state.
	session *SessionState
	// The currently tracked shared generation.
	generation uint64
	// The server we aggregate the data for (logging and debugging purposes only)
	server string
	logger logging.Logger
}

// NewAggregate create new client aggregator.
func NewAggregate(server string, session *SessionState, logger logging.Logger) *Aggregate {
	generation := uint64(0)
	if session != nil {
		generation = session.Snapshot().Generation
	}

	return &Aggregate{
		group:      mapr.NewGroupSet(logger),
		session:    session,
		generation: generation,
		server:     server,
		logger:     logging.OrNop(logger),
	}
}

// Aggregate data from mapr log line into local (and global) group sets.
func (a *Aggregate) Aggregate(message string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session == nil {
		return fmt.Errorf("missing client mapreduce session state")
	}

	snapshot := a.session.Snapshot()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		return fmt.Errorf("missing client mapreduce query state")
	}
	if snapshot.Generation != a.generation {
		a.group.InitSet()
		a.generation = snapshot.Generation
	}

	parts := strings.Split(message, protocol.AggregateDelimiter)
	if len(parts) < 4 {
		return fmt.Errorf("aggregate message without any real data")
	}

	groupKey := parts[0]
	samples, parseErr := strconv.Atoi(parts[1])
	if parseErr != nil {
		return fmt.Errorf("unable to parse sample count '%s': %w", parts[1], parseErr)
	}

	fields := a.makeFields(parts[2:])
	set := a.group.GetSet(groupKey)
	var addedSamples bool

	for _, sc := range snapshot.Query.Select {
		if val, ok := fields[sc.FieldStorage]; ok {
			if aggregateErr := set.Aggregate(sc.FieldStorage, sc.Operation, val, true); aggregateErr != nil {
				a.logger.Error(aggregateErr)
				continue
			}
			addedSamples = true
		}
	}
	if addedSamples {
		set.Samples += samples
	}

	// Merge data from group into global group.
	isMerged, err := snapshot.GlobalGroup.MergeNoblock(snapshot.Query, a.group)
	if err != nil {
		return fmt.Errorf("unable to merge aggregate data for server %s: %w", a.server, err)
	}
	if isMerged {
		// Re-init local group (make it empty again).
		a.group.InitSet()
	}
	return nil
}

// Flush merges any pending per-server aggregate state into the shared global group.
// The normal hot path uses MergeNoblock to avoid stalling on the global merge lock.
// During shutdown we need a blocking flush so the last local batch is not lost.
func (a *Aggregate) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session == nil {
		return fmt.Errorf("missing client mapreduce session state")
	}

	snapshot := a.session.Snapshot()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		return nil
	}
	if snapshot.Generation != a.generation {
		a.group.InitSet()
		a.generation = snapshot.Generation
		return nil
	}

	if err := snapshot.GlobalGroup.Merge(snapshot.Query, a.group); err != nil {
		return fmt.Errorf("unable to flush aggregate data for server %s: %w", a.server, err)
	}
	a.group.InitSet()
	return nil
}

// Create a map of key-value pairs from a part list such as ["foo=bar",  "bar=baz"].
func (a *Aggregate) makeFields(parts []string) map[string]string {
	fields := make(map[string]string, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(part, protocol.AggregateKVDelimiter)
		if !ok {
			continue
		}
		fields[key] = value
	}
	return fields
}
