package client

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// SessionSnapshot captures the current client-side mapreduce session state.
type SessionSnapshot struct {
	Generation  uint64
	Query       *mapr.Query
	GlobalGroup *mapr.GlobalGroupSet
	LastResult  string
	// AggregatesArrived is signaled (buffered, so several signals coalesce)
	// whenever a server's first aggregate messages of this generation were
	// merged into GlobalGroup (Aggregate.Merge or Flush), so the reporter
	// can show a query's first result right after its data arrived instead
	// of waiting for its next report tick. It is nil without a query, so a
	// receive from it then blocks.
	AggregatesArrived <-chan struct{}
	// arrival signals AggregatesArrived and counts the servers behind it
	// (ArrivedFrom); nil without a query.
	arrival *aggregateArrival
}

// aggregateArrival signals, per query generation, each server whose first
// aggregate messages were merged.
type aggregateArrival struct {
	mu      sync.Mutex
	servers map[string]struct{}
	ch      chan struct{}
}

func newAggregateArrival() *aggregateArrival {
	return &aggregateArrival{servers: make(map[string]struct{}), ch: make(chan struct{}, 1)}
}

// mark signals the arrival's channel the first time it is called for
// server; a nil arrival (no query) is ignored. It never blocks: a signal
// the reporter has not taken yet stands for this one too.
func (a *aggregateArrival) mark(server string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	_, seen := a.servers[server]
	if !seen {
		a.servers[server] = struct{}{}
	}
	a.mu.Unlock()
	if seen {
		return
	}
	select {
	case a.ch <- struct{}{}:
	default:
	}
}

// has reports whether server's aggregates arrived; false for a nil
// arrival.
func (a *aggregateArrival) has(server string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.servers[server]
	return ok
}

// channel returns the channel mark signals, nil for a nil arrival.
func (a *aggregateArrival) channel() <-chan struct{} {
	if a == nil {
		return nil
	}
	return a.ch
}

// SessionState keeps the mutable mapreduce query state shared by the client
// reporter and per-server handlers.
type SessionState struct {
	mu sync.RWMutex
	// generation is only changed under mu, together with query and global,
	// but may be read without it (Generation).
	generation atomic.Uint64
	query      *mapr.Query
	global     *mapr.GlobalGroupSet
	lastResult string
	// arrival signals each server's first merged aggregates of the
	// generation (see SessionSnapshot.AggregatesArrived); replaced with the
	// query.
	arrival   *aggregateArrival
	changedCh chan struct{}
	logger    logging.Logger
	// aggregatesExpected tells the handlers whether the servers may send
	// aggregate frames (AggregatesExpected); read without mu, once per
	// frame.
	aggregatesExpected atomic.Bool
	// pendingSwitch numbers the switch of the session to another workload
	// that is in progress (Switching), 0 while none is; switches counts
	// them. holders are the connections' aggregates holding messages for
	// it (holdFor), in the order they began to.
	pendingSwitch uint64
	switches      uint64
	holders       []*Aggregate
	// switchCommitted is set once the pending switch committed a workload
	// (CommitQuery, ClearQuery); endedCommitted keeps it for the switch
	// that ended last (switchOutcome).
	switchCommitted bool
	endedCommitted  bool
}

// NewSessionState returns a new shared mapreduce session state. A nil query
// starts it without a query (see ClearQuery), as interactive dtail does when
// it starts with a plain tail read.
func NewSessionState(query *mapr.Query, logger logging.Logger) *SessionState {
	logger = logging.OrNop(logger)
	state := &SessionState{
		query:     query,
		changedCh: make(chan struct{}, 1),
		logger:    logger,
	}
	if query != nil {
		state.global = mapr.NewGlobalGroupSet(logger)
		state.arrival = newAggregateArrival()
		state.aggregatesExpected.Store(true)
	}
	return state
}

// AggregatesExpected reports whether the servers may send aggregate frames:
// while a query is committed, or while a switch to one is pending
// (ExpectAggregates). A handler takes a payload frame for aggregate data only
// then, so that a followed --plain line that starts like an aggregate frame
// (AGGREGATE|...) is printed while interactive dtail follows lines. After a
// switch back to following lines no aggregate frame arrives any more: a
// server sends none of the replaced generation after it acknowledged the
// update, and the client clears the query (ClearQuery) only after every
// acknowledgement.
func (s *SessionState) AggregatesExpected() bool {
	return s.aggregatesExpected.Load()
}

// ExpectAggregates is called with true before the session is switched to a
// query, so that the first aggregates of the query, which may arrive before
// CommitQuery, are taken for aggregate data, and with false when that switch
// failed: then only a committed query expects them.
func (s *SessionState) ExpectAggregates(pending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggregatesExpected.Store(pending || s.query != nil)
}

// Switching is called with true before the client updates the sessions of
// its connections to another workload (an interactive reload) and with
// false once that ended, committed (CommitQuery, ClearQuery) or rolled
// back. While it is pending, a connection's Aggregate holds the aggregates
// its server sends after it acknowledged the update. At the end they are
// merged into the result that is current then, or, after a switch that
// committed nothing, dropped unless the server was rolled back (see
// Aggregate.SessionAcked and settleHold).
//
// Switching(false) returns only after every one of them settled what it
// held, each waiting for the global group set's lock in turn: whoever
// reads the result after the reload (the next report, the final result
// of a client whose sessions ended meanwhile) finds it complete, and the
// holders do not all retry for the lock at once. mu is not held while
// they merge, as a merge takes a Snapshot.
func (s *SessionState) Switching(pending bool) {
	if pending {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pendingSwitch == 0 {
			s.switches++
			s.pendingSwitch = s.switches
		}
		return
	}
	for _, holder := range s.endSwitch() {
		holder.endHold()
	}
}

// endSwitch ends the pending switch, if any, remembers whether it
// committed a workload, and returns the aggregates that hold messages for
// it.
func (s *SessionState) endSwitch() []*Aggregate {
	s.mu.Lock()
	defer s.mu.Unlock()
	holders := s.holders
	if s.pendingSwitch != 0 {
		s.endedCommitted = s.switchCommitted
	}
	s.pendingSwitch, s.holders, s.switchCommitted = 0, nil, false
	return holders
}

// switchOutcome reports whether the switch numbered id is still pending
// and, if it ended, whether it committed a workload (false for one that
// was rejected or rolled back). Every holder of a switch is settled at
// its end, before the next one begins, so an ended id is that of the
// switch that ended last.
func (s *SessionState) switchOutcome(id uint64) (pending, committed bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id == s.pendingSwitch {
		return true, false
	}
	return false, s.endedCommitted
}

// switchPending returns the number of the pending switch (Switching), 0
// when none is pending.
func (s *SessionState) switchPending() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pendingSwitch
}

// holdFor registers holder as holding messages for the pending switch,
// whose end then makes it merge them (Aggregate.endHold), and returns the
// switch's number; 0, registering nothing, when no switch is pending.
func (s *SessionState) holdFor(holder *Aggregate) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingSwitch != 0 && !slices.Contains(s.holders, holder) {
		s.holders = append(s.holders, holder)
	}
	return s.pendingSwitch
}

// Snapshot returns a point-in-time copy of the shared mapreduce state.
func (s *SessionState) Snapshot() SessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return SessionSnapshot{
		Generation:        s.generation.Load(),
		Query:             s.query,
		GlobalGroup:       s.global,
		LastResult:        s.lastResult,
		AggregatesArrived: s.arrival.channel(),
		arrival:           s.arrival,
	}
}

// WithCurrentSnapshot calls fn only while snapshot still identifies the active
// query generation. The read lock prevents CommitQuery from completing during
// fn. The callback must not call another SessionState method.
func (s *SessionState) WithCurrentSnapshot(snapshot SessionSnapshot, fn func() error) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.generation.Load() != snapshot.Generation || s.query != snapshot.Query || s.global != snapshot.GlobalGroup {
		return false, nil
	}
	return true, fn()
}

// Generation returns the current query generation without taking the lock,
// so that a per-message caller (Aggregate.Add) does not contend for it with
// every other server connection. The query and global group set of that
// generation must still be taken from a Snapshot.
func (s *SessionState) Generation() uint64 {
	return s.generation.Load()
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
	s.generation.Store(generation)
	s.query = query
	s.global = mapr.NewGlobalGroupSet(s.logger)
	s.lastResult = ""
	s.arrival = newAggregateArrival()
	s.aggregatesExpected.Store(true)
	s.switchCommitted = s.pendingSwitch != 0
	s.mu.Unlock()

	s.notifyChange()
	return query, nil
}

// ClearQuery switches the shared state to generation without a query, for a
// client whose session went from a MapReduce query back to a plain read
// (interactive dtail, see clients.MaprClient). The query's global group set
// and last rendered result are dropped with it: Snapshot then returns a nil
// Query and GlobalGroup, so nothing reports a result; payload frames are
// no longer taken for aggregate data (AggregatesExpected), and aggregates
// still queued are discarded (Aggregate.merge), until CommitQuery sets a new
// query.
func (s *SessionState) ClearQuery(generation uint64) {
	s.mu.Lock()
	s.generation.Store(generation)
	s.query = nil
	s.global = nil
	s.lastResult = ""
	s.arrival = nil
	s.aggregatesExpected.Store(false)
	s.switchCommitted = s.pendingSwitch != 0
	s.mu.Unlock()

	s.notifyChange()
}

// CommitRenderedResult stores the last rendered result for the active generation.
func (s *SessionState) CommitRenderedResult(generation uint64, result string) (changed bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.generation.Load() != generation {
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

// ArrivedFrom reports whether server's aggregates of the snapshot's
// generation have arrived (server as its MaprHandler names it, the
// connection's Server()).
func (s SessionSnapshot) ArrivedFrom(server string) bool {
	return s.arrival.has(server)
}
