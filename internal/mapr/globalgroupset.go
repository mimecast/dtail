package mapr

import (
	"fmt"
)

// GlobalGroupSet is used on the dtail client to merge multiple group sets
// (one group set per remote server) to one single global group set.
type GlobalGroupSet struct {
	GroupSet
	semaphore chan struct{}
}

// NewGlobalGroupSet creates a new empty global group set.
func NewGlobalGroupSet() *GlobalGroupSet {
	g := GlobalGroupSet{
		semaphore: make(chan struct{}, 1),
	}
	g.InitSet()

	return &g
}

// String representation of the global group set.
func (g *GlobalGroupSet) String() string {
	return fmt.Sprintf("GlobalGroupSet(%s)", g.GroupSet.String())
}

// Merge (blocking) a group set into the global group set.
func (g *GlobalGroupSet) Merge(query *Query, group *GroupSet) error {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()

	return g.merge(query, group)
}

// MergeNoblock merges (non-blocking) a group set into the global group set.
func (g *GlobalGroupSet) MergeNoblock(query *Query, group *GroupSet) (bool, error) {
	select {
	case g.semaphore <- struct{}{}:
		err := g.merge(query, group)
		<-g.semaphore
		return true, err
	default:
		return false, nil
	}
}

// Merge a group set into the global group set.
func (g *GlobalGroupSet) merge(query *Query, group *GroupSet) error {

	for groupKey, set := range group.sets {
		s := g.GetSet(groupKey)
		if err := s.Merge(query, set); err != nil {
			return err
		}
	}

	return nil
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

	set := &GroupSet{sets: g.sets}
	g.InitSet()

	return set
}

// WriteResult writes the result of a mapreduce aggregation to an outfile.
func (g *GlobalGroupSet) WriteResult(query *Query) error {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()

	return g.GroupSet.WriteResult(query)
}

// Result returns the result of the mapreduce aggregation as a string.
func (g *GlobalGroupSet) Result(query *Query, rowsLimit int) (string, int, error) {
	g.semaphore <- struct{}{}
	defer func() { <-g.semaphore }()

	return g.GroupSet.Result(query, rowsLimit)
}
