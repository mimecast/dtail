package client

import (
	"fmt"
	"sync"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// SessionSnapshot captures the current client-side mapreduce session state.
type SessionSnapshot struct {
	Generation  uint64
	Query       *mapr.Query
	GlobalGroup *mapr.GlobalGroupSet
	LastResult  string
}

// SessionState keeps the mutable mapreduce query state shared by the client
// reporter and per-server handlers.
type SessionState struct {
	mu         sync.RWMutex
	generation uint64
	query      *mapr.Query
	global     *mapr.GlobalGroupSet
	lastResult string
	changedCh  chan struct{}
	logger     logging.Logger
}

// NewSessionState returns a new shared mapreduce session state.
func NewSessionState(query *mapr.Query, logger logging.Logger) *SessionState {
	logger = logging.OrNop(logger)
	return &SessionState{
		query:     query,
		global:    mapr.NewGlobalGroupSet(logger),
		changedCh: make(chan struct{}, 1),
		logger:    logger,
	}
}

// Snapshot returns a point-in-time copy of the shared mapreduce state.
func (s *SessionState) Snapshot() SessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return SessionSnapshot{
		Generation:  s.generation,
		Query:       s.query,
		GlobalGroup: s.global,
		LastResult:  s.lastResult,
	}
}

// WithCurrentSnapshot calls fn only while snapshot still identifies the active
// query generation. The read lock prevents CommitQuery from completing during
// fn. The callback must not call another SessionState method.
func (s *SessionState) WithCurrentSnapshot(snapshot SessionSnapshot, fn func() error) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.generation != snapshot.Generation || s.query != snapshot.Query || s.global != snapshot.GlobalGroup {
		return false, nil
	}
	return true, fn()
}

// Changes returns a channel that is signaled whenever a new generation is committed.
func (s *SessionState) Changes() <-chan struct{} {
	return s.changedCh
}

// CommitQuery resets the shared aggregation state for a newly accepted query generation.
func (s *SessionState) CommitQuery(rawQuery string, generation uint64) (*mapr.Query, error) {
	query, err := mapr.NewQuery(rawQuery, s.logger)
	if err != nil {
		return nil, fmt.Errorf("parse session query: %w", err)
	}

	s.mu.Lock()
	s.generation = generation
	s.query = query
	s.global = mapr.NewGlobalGroupSet(s.logger)
	s.lastResult = ""
	s.mu.Unlock()

	s.notifyChange()
	return query, nil
}

// CommitRenderedResult stores the last rendered result for the active generation.
func (s *SessionState) CommitRenderedResult(generation uint64, result string) (changed bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.generation != generation {
		return false, false
	}
	if s.lastResult == result {
		return false, true
	}

	s.lastResult = result
	return true, true
}

func (s *SessionState) notifyChange() {
	select {
	case s.changedCh <- struct{}{}:
	default:
	}
}
