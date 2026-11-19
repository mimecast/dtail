package client

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	switchQueryA = "select status,count(status) from stats group by status"
	switchQueryB = "select status,count(status) from warnings group by status"
)

// newSwitchAggregate returns the aggregate of server srv1 on a session
// running switchQueryA, and the count storage of its messages.
func newSwitchAggregate(t *testing.T) (*SessionState, *Aggregate, string) {
	t.Helper()
	query := mustSessionStateQuery(t, switchQueryA)
	state := NewSessionState(query, logging.NopLogger{})
	return state, NewAggregate("srv1", state, logging.NopLogger{}), aggregateCountStorage(t, query)
}

// groupCount returns the count() of group key in snapshot's result, and
// whether the group exists.
func groupCount(t *testing.T, snapshot SessionSnapshot, key string) (float64, bool) {
	t.Helper()
	storage := aggregateCountStorage(t, snapshot.Query)
	var count float64
	var found bool
	before := snapshot.GlobalGroup.NumSets()
	snapshot.GlobalGroup.Update(func(group *mapr.GroupSet) {
		count = group.GetSet(key).FValues[storage]
	})
	found = snapshot.GlobalGroup.NumSets() == before
	return count, found
}

// requireCount fails unless the group key of snapshot counts want.
func requireCount(t *testing.T, snapshot SessionSnapshot, key string, want float64) {
	t.Helper()
	if got, _ := groupCount(t, snapshot, key); got != want {
		t.Fatalf("count of %q = %v, want %v", key, got, want)
	}
}

// TestAggregateHoldsMessagesOfAckedSwitch: during a switch of the session,
// what a server sent before its acknowledgement belongs to the replaced
// query, what it sends after it to the new one, although that is not
// committed yet. Merge and Flush keep those messages, also past the
// commit, until the switch ended: its end merges them into the new
// query's result itself, without a further message arriving.
func TestAggregateHoldsMessagesOfAckedSwitch(t *testing.T) {
	state, aggregate, storage := newSwitchAggregate(t)
	replaced := state.Snapshot()

	state.Switching(true)
	addMessage(t, aggregate, countMessage("OLD", storage, "1"))
	aggregate.SessionAcked()
	requireCount(t, replaced, "OLD", 1)

	addMessage(t, aggregate, countMessage("NEW", storage, "2"))
	mergeMessages(t, aggregate)
	if err := aggregate.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if _, err := state.CommitQuery(switchQueryB, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	addMessage(t, aggregate, countMessage("NEW", storage, "3"))
	mergeMessages(t, aggregate)
	requireCount(t, state.Snapshot(), "NEW", 0)

	state.Switching(false)
	requireCount(t, state.Snapshot(), "NEW", 5)
	if _, found := groupCount(t, replaced, "NEW"); found {
		t.Fatal("the replaced query's result got a group of the new query")
	}
	// After the switch nothing is held any more.
	addMessage(t, aggregate, countMessage("NEW", storage, "1"))
	mergeMessages(t, aggregate)
	requireCount(t, state.Snapshot(), "NEW", 6)
}

// TestAggregateDropsHeldMessagesOnSecondAck: a second acknowledgement
// within one switch is the rollback's. What was held belongs to the
// workload that was abandoned and is dropped; what follows is the previous
// query's again and is merged into its result once the switch ended.
func TestAggregateDropsHeldMessagesOnSecondAck(t *testing.T) {
	state, aggregate, storage := newSwitchAggregate(t)

	state.Switching(true)
	aggregate.SessionAcked()
	addMessage(t, aggregate, countMessage("ABANDONED", storage, "4"))
	mergeMessages(t, aggregate)
	aggregate.SessionAcked()
	addMessage(t, aggregate, countMessage("OLD", storage, "1"))
	mergeMessages(t, aggregate)
	requireCount(t, state.Snapshot(), "OLD", 0)

	state.Switching(false)
	requireCount(t, state.Snapshot(), "OLD", 1)
	if got := state.Snapshot().GlobalGroup.NumSets(); got != 1 {
		t.Fatalf("NumSets() = %d, want only the group sent after the rollback", got)
	}
}

// TestAggregateHoldsNothingOutsideSwitch: an acknowledgement while no
// switch is pending (the initial SESSION START) holds nothing, and a
// message queued before a commit is still dropped as of an earlier
// generation.
func TestAggregateHoldsNothingOutsideSwitch(t *testing.T) {
	state, aggregate, storage := newSwitchAggregate(t)

	aggregate.SessionAcked()
	addMessage(t, aggregate, countMessage("ERROR", storage, "1"))
	mergeMessages(t, aggregate)
	requireCount(t, state.Snapshot(), "ERROR", 1)

	addMessage(t, aggregate, countMessage("STALE", storage, "1"))
	if _, err := state.CommitQuery(switchQueryB, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	mergeMessages(t, aggregate)
	if got := state.Snapshot().GlobalGroup.NumSets(); got != 0 {
		t.Fatalf("NumSets() = %d after merging a stale message, want 0", got)
	}
}

// TestAggregateHeldMessagesOfEndedSwitch covers what is still held when the
// next acknowledgement arrives after the switch ended: a later switch's
// acknowledgement merges it into the result the ended switch committed
// instead of holding it for the later switch, and the query cleared at
// the end of a switch (interactive dtail back to following lines) drops it.
func TestAggregateHeldMessagesOfEndedSwitch(t *testing.T) {
	state, aggregate, storage := newSwitchAggregate(t)

	state.Switching(true)
	aggregate.SessionAcked()
	addMessage(t, aggregate, countMessage("FIRST", storage, "2"))
	if _, err := state.CommitQuery(switchQueryB, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	committed := state.Snapshot()
	state.Switching(false)
	state.Switching(true)
	aggregate.SessionAcked()
	requireCount(t, committed, "FIRST", 2)

	addMessage(t, aggregate, countMessage("SECOND", storage, "1"))
	state.ClearQuery(3)
	state.Switching(false)
	mergeMessages(t, aggregate)
	if snapshot := state.Snapshot(); snapshot.Query != nil || snapshot.GlobalGroup != nil {
		t.Fatalf("session after ClearQuery = %#v, want no query", snapshot)
	}
	if _, found := groupCount(t, committed, "SECOND"); found {
		t.Fatal("a message held for a switch to no query was merged into the replaced result")
	}
}

// TestSessionStateSwitchingPending checks switchPending: 0 outside a
// switch, the same number within one (also when it is begun twice), 0
// after its end, and another number for the next switch.
func TestSessionStateSwitchingPending(t *testing.T) {
	state := NewSessionState(nil, logging.NopLogger{})
	if state.switchPending() != 0 {
		t.Fatal("a switch is pending on a new session state")
	}
	state.Switching(false)
	state.Switching(true)
	first := state.switchPending()
	state.Switching(true)
	if first == 0 || state.switchPending() != first {
		t.Fatal("beginning a pending switch again replaced it")
	}
	state.Switching(false)
	if state.switchPending() != 0 {
		t.Fatal("a switch is still pending after its end")
	}
	state.Switching(true)
	if second := state.switchPending(); second == 0 || second == first {
		t.Fatal("the next switch has no number of its own")
	}
}

// TestSwitchEndWaitsForHeldMessages: the end of a switch returns only
// after the held messages are in the result, also while another
// connection (or a rendering) holds the global group set's lock, and also
// of a connection whose session ended during the switch, whose shutdown
// Flush keeps what is held. Whoever reads the result after the reload,
// e.g. the client's final report, must find those rows.
func TestSwitchEndWaitsForHeldMessages(t *testing.T) {
	state, ended, storage := newSwitchAggregate(t)
	other := NewAggregate("srv2", state, logging.NopLogger{})

	state.Switching(true)
	for i, aggregate := range []*Aggregate{ended, other} {
		aggregate.SessionAcked()
		addMessage(t, aggregate, countMessage("NEW", storage, strconv.Itoa(i+1)))
		mergeMessages(t, aggregate)
	}
	// The session of srv1 ends: its handler flushes on shutdown.
	if err := ended.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if _, err := state.CommitQuery(switchQueryB, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	committed := state.Snapshot()

	locked, unlock, switched := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go committed.GlobalGroup.Update(func(*mapr.GroupSet) {
		close(locked)
		<-unlock
	})
	<-locked
	go func() {
		state.Switching(false)
		close(switched)
	}()
	select {
	case <-switched:
		t.Fatal("the switch ended while the result's lock was held, before the held messages were merged")
	case <-time.After(50 * time.Millisecond):
	}
	close(unlock)
	<-switched
	requireCount(t, committed, "NEW", 3)
}

// TestAggregateHoldsLargeResult: the hold is not capped. A server that
// sends a large result behind its acknowledgement (200,000 groups, far
// more than maxPendingBytes, at which a capped hold dropped most of them)
// has all of it in the reloaded query's result.
func TestAggregateHoldsLargeResult(t *testing.T) {
	const groups = 200000
	state, aggregate, storage := newSwitchAggregate(t)
	replaced := state.Snapshot()

	state.Switching(true)
	aggregate.SessionAcked()
	for i := range groups {
		addMessage(t, aggregate, countMessage("group-"+strconv.Itoa(i), storage, "1"))
		if i%500 == 499 {
			// The end of a Write call of the handler.
			mergeMessages(t, aggregate)
		}
	}
	if len(aggregate.pending) < maxPendingBytes {
		t.Fatalf("held %d bytes, want more than maxPendingBytes for this test", len(aggregate.pending))
	}
	if _, err := state.CommitQuery(switchQueryB, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	state.Switching(false)
	if got := state.Snapshot().GlobalGroup.NumSets(); got != groups {
		t.Fatalf("NumSets() = %d in the reloaded query's result, want %d", got, groups)
	}
	if got := replaced.GlobalGroup.NumSets(); got != 0 {
		t.Fatalf("NumSets() = %d in the replaced query's result, want 0", got)
	}
}

// TestFailedSwitchDropsAbandonedMessages: a switch that ended without a
// commit (another server rejected the update) leaves the previous query
// in place. What a connection holds without a second acknowledgement, as
// its session ended before the rollback reached it, is the abandoned
// query's and must not be merged into the result that stays, neither by
// the end of the switch nor by a later flush; a connection that was
// rolled back keeps what it sent after.
func TestFailedSwitchDropsAbandonedMessages(t *testing.T) {
	state, ended, storage := newSwitchAggregate(t)
	rolledBack := NewAggregate("srv2", state, logging.NopLogger{})

	state.Switching(true)
	ended.SessionAcked()
	addMessage(t, ended, countMessage("ABANDONED", storage, "5"))
	if err := ended.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	rolledBack.SessionAcked()
	addMessage(t, rolledBack, countMessage("ABANDONED", storage, "5"))
	rolledBack.SessionAcked()
	addMessage(t, rolledBack, countMessage("OLD", storage, "1"))
	state.Switching(false)

	if err := ended.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	requireCount(t, state.Snapshot(), "OLD", 1)
	if got := state.Snapshot().GlobalGroup.NumSets(); got != 1 {
		t.Fatalf("NumSets() = %d, want only the group sent after the rollback", got)
	}
}

// blockingLogger blocks every error forever, as a paused client logger.
type blockingLogger struct {
	logging.NopLogger
	block chan struct{}
}

func (l blockingLogger) Error(...any) string {
	<-l.block
	return ""
}

// errorCountLogger counts the errors logged.
type errorCountLogger struct {
	logging.NopLogger
	errors *atomic.Int32
}

func (l errorCountLogger) Error(...any) string {
	l.errors.Add(1)
	return ""
}

// TestEndHoldLogsFailuresToTheSessionLogger: the end of a switch merges
// what a connection held, on the interactive control loop. An aggregation
// failure of it must not be logged to the connection's logger, which blocks
// while the client's output is paused (the control loop alone could end
// that pause); it goes to the session state's logger.
func TestEndHoldLogsFailuresToTheSessionLogger(t *testing.T) {
	query := mustSessionStateQuery(t, "select status,count(status) from stats group by status")
	var failures atomic.Int32
	state := NewSessionState(query, errorCountLogger{errors: &failures})
	aggregate := NewAggregate("srv1", state, blockingLogger{block: make(chan struct{})})

	state.Switching(true)
	aggregate.SessionAcked()
	addMessage(t, aggregate, strings.Join([]string{"ERROR", "1",
		aggregateCountStorage(t, query) + protocol.AggregateKVDelimiter + "not a number", ""}, protocol.AggregateDelimiter))
	if _, err := state.CommitQuery(query.RawQuery, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		state.Switching(false)
	}()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the end of the switch blocked in the connection's logger")
	}
	if got := failures.Load(); got != 1 {
		t.Fatalf("session logger got %d failures, want 1", got)
	}
}
