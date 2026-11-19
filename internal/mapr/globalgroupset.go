package mapr

import (
	"fmt"

	"github.com/mimecast/dtail/internal/logging"
)

// GlobalGroupSet is used on the dtail client to merge multiple group sets
// (one group set per remote server) to one single global group set.
//
// The client aggregates into it with Update and UpdateNoblock only (see
// client.Aggregate): each received message is added straight to the global
// set's GetSetBytes entry, as merging a set holding only that message with
// AggregateSet.Merge would; no per-message group set is built and merged as a
// whole.
type GlobalGroupSet struct {
	GroupSet
	semaphore chan struct{}
}

// NewGlobalGroupSet creates a new empty global group set.
func NewGlobalGroupSet(logger logging.Logger) *GlobalGroupSet {
	g := GlobalGroupSet{
		GroupSet:  *NewGroupSet(logger),
		semaphore: make(chan struct{}, 1),
	}
	return &g
}

// String representation of the global group set.
func (g *GlobalGroupSet) String() string {
	return fmt.Sprintf("GlobalGroupSet(%s)", g.GroupSet.String())
}

// Update calls fn with the global group set while it holds the global group
// set's lock, waiting for the lock if another merge or a result rendering
// holds it. fn may aggregate into the group set (GetSet, GetSetBytes) but must
// not call another GlobalGroupSet method, which would wait for the same lock.
func (g *GlobalGroupSet) Update(fn func(*GroupSet)) {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()
	fn(&g.GroupSet)
}

// UpdateNoblock calls fn like Update, but only when the lock is free right
// away, and reports whether it did. The client aggregates a server's messages
// with it, so a connection never waits for another one or for the result
// rendering; it keeps its messages and tries again later instead. The lock is
// released via defer so it is returned even if fn panics, as in Update.
func (g *GlobalGroupSet) UpdateNoblock(fn func(*GroupSet)) bool {
	select {
	case g.semaphore <- struct{}{}:
		defer func() { <-g.semaphore }()
		fn(&g.GroupSet)
		return true
	default:
		return false
	}
}

// IsEmpty determines whether the global group set has any data in it.
func (g *GlobalGroupSet) IsEmpty() bool {
	return g.NumSets() == 0
}

// NumSets determines the number of sets.
func (g *GlobalGroupSet) NumSets() int {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()
	return len(g.sets)
}

// SwapOut teturn the underlying group set and create a new empty one, so
// that the global group set is empty again and can aggregate new data.
func (g *GlobalGroupSet) SwapOut() *GroupSet {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()

	set := &GroupSet{sets: g.sets, logger: g.logger}
	g.InitSet()
	return set
}

// WriteResult writes the result of a mapreduce aggregation to an outfile.
func (g *GlobalGroupSet) WriteResult(query *Query, finalResult bool) error {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()
	return g.GroupSet.WriteResult(query, finalResult)
}

// Result returns the result of the mapreduce aggregation as a string.
func (g *GlobalGroupSet) Result(query *Query, rowsLimit int, renderer ResultRenderer) (string, int, error) {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()
	return g.GroupSet.Result(query, rowsLimit, renderer)
}
