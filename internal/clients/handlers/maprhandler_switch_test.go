package handlers

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// writeAckAndAggregate writes the control frame ack followed by one
// aggregate frame of group key (count 1) to handler in one Write, as a
// server sends the first result of a workload right behind its
// acknowledgement.
func writeAckAndAggregate(t *testing.T, handler *MaprHandler, query *mapr.Query, ack, key string) {
	t.Helper()
	serialized := strings.Join([]string{key, "1",
		handlerCountStorage(t, query) + protocol.AggregateKVDelimiter + "1", ""}, protocol.AggregateDelimiter)
	var input bytes.Buffer
	protocol.EscapedControlFraming.EncodeControl(&input, ack)
	protocol.EscapedControlFraming.EncodePayload(&input, []byte(protocol.AggregateMessageID+
		protocol.FieldSeparator()+"host1"+protocol.FieldSeparator()+serialized+"\n"))
	if _, err := handler.Write(input.Bytes()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

// groupSets returns the number of groups of the session's current result.
func groupSets(session *maprclient.SessionState) int {
	return session.Snapshot().GlobalGroup.NumSets()
}

// TestMaprHandlerHoldsAggregatesBehindSessionAck: while the client switches
// the session, the aggregates behind a server's acknowledgement of the
// update are those of the workload not committed yet. The handler keeps
// them out of the current result and the end of the switch merges them. Behind a rejection the server still runs the current workload,
// so its aggregates are merged at once; so are those behind an
// acknowledgement while no switch is pending.
func TestMaprHandlerHoldsAggregatesBehindSessionAck(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	session := maprclient.NewSessionState(query, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, &payloadRecordingLogger{})
	writeCapabilitiesAndLines(t, handler, nil)

	writeAckAndAggregate(t, handler, query, protocol.HiddenSessionStartOKPrefix+" 1", "START")
	if got := groupSets(session); got != 1 {
		t.Fatalf("groups = %d after an acknowledgement outside a switch, want 1", got)
	}

	session.Switching(true)
	writeAckAndAggregate(t, handler, query, protocol.HiddenSessionErrorPrefix+"rejected", "REJECTED")
	if got := groupSets(session); got != 2 {
		t.Fatalf("groups = %d after a rejected update, want 2", got)
	}
	writeAckAndAggregate(t, handler, query, protocol.HiddenSessionUpdateOKPrefix+" 2", "UPDATED")
	if got := groupSets(session); got != 2 {
		t.Fatalf("groups = %d while the switch is pending, want the new workload's aggregate held", got)
	}
	// The acknowledgements reached the connection as well, in order.
	for _, want := range []string{"start", "error", "update"} {
		if ack, ok := handler.WaitForSessionAck(time.Second, time.Second); !ok || ack.Action != want {
			t.Fatalf("acknowledgement = %#v (%v), want action %q", ack, ok, want)
		}
	}

	// The client commits the query the update switched to: the held
	// aggregate is the first of its result.
	if _, err := session.CommitQuery(query.RawQuery, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	session.Switching(false)
	if got := groupSets(session); got != 1 {
		t.Fatalf("groups = %d in the committed query's result after the switch ended, want 1", got)
	}
}

// TestMaprHandlerDropsAbandonedStartWorkload: a failed reload could not
// wait for the acknowledgement of a connection's SESSION START, which it
// had given the reloaded spec, so the start rolls its session back with an
// update right after its acknowledgement. Both acknowledgements arrive
// outside the reload's switch. The aggregates between them are the
// reloaded query's and must not get into the result of the query the
// client kept (they used to be merged into it); those behind the
// rollback's acknowledgement are the kept query's again.
func TestMaprHandlerDropsAbandonedStartWorkload(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	session := maprclient.NewSessionState(query, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, &payloadRecordingLogger{})
	writeCapabilitiesAndLines(t, handler, nil)

	handler.AbandonStartWorkload(true)
	writeAckAndAggregate(t, handler, query, protocol.HiddenSessionStartOKPrefix+" 1", "RELOADED")
	if got := groupSets(session); got != 0 {
		t.Fatalf("groups = %d behind the abandoned start's acknowledgement, want 0", got)
	}
	writeAckAndAggregate(t, handler, query, protocol.HiddenSessionUpdateOKPrefix+" 2", "KEPT")
	if got := groupSets(session); got != 1 {
		t.Fatalf("groups = %d behind the rollback's acknowledgement, want the kept query's 1", got)
	}
	if err := handler.aggregate.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := groupSets(session); got != 1 {
		t.Fatalf("groups = %d after a flush, want the abandoned workload's still dropped", got)
	}
}

// TestMaprHandlerAbandonTakenBack: the start finished in the moment the
// rollback gave up on it, so the abandon is taken back and the session is
// rolled back like any committed one; nothing is dropped outside a switch
// then, and nothing without an abandon at all.
func TestMaprHandlerAbandonTakenBack(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	session := maprclient.NewSessionState(query, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, &payloadRecordingLogger{})
	writeCapabilitiesAndLines(t, handler, nil)

	handler.AbandonStartWorkload(true)
	handler.AbandonStartWorkload(false)
	writeAckAndAggregate(t, handler, query, protocol.HiddenSessionStartOKPrefix+" 1", "START")
	if got := groupSets(session); got != 1 {
		t.Fatalf("groups = %d after the abandon was taken back, want 1", got)
	}
}
