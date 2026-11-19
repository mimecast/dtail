package connectors

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
)

// TestSessionUpdateSkipsStaleAck: a SESSION UPDATE that timed out is rolled
// back with another SESSION UPDATE, numbered one above it. The
// acknowledgement of the timed-out update can still arrive after the
// rollback was sent (the acknowledgements queued before it are drained, but
// not those arriving later). It has a lower generation than the rollback and
// must be skipped, so that the rollback's own answer decides: before, the
// late acknowledgement was either taken for the rollback's (same number,
// rollback reported as done while the server rejected it) or failed it as
// unexpected (numbers differing).
func TestSessionUpdateSkipsStaleAck(t *testing.T) {
	late := handlers.SessionAck{Action: "update", Generation: 5}
	tests := []struct {
		name          string
		acks          []handlers.SessionAck
		wantErr       error
		wantCommitted uint64
	}{
		{name: "late ack then rejection", wantErr: ErrSessionRejected, wantCommitted: 4,
			acks: []handlers.SessionAck{late, {Action: "error", Error: "unable to build commands"}}},
		{name: "late ack then the update's ack", wantCommitted: 6,
			acks: []handlers.SessionAck{late, {Action: "update", Generation: 6}}},
		{name: "several late acks", wantCommitted: 6,
			acks: []handlers.SessionAck{{Action: "update", Generation: 3}, late, {Action: "update", Generation: 6}}},
		{name: "late ack only", wantErr: ErrSessionAckTimeout, wantCommitted: 4,
			acks: []handlers.SessionAck{late}},
		{name: "ack of a later generation", wantErr: ErrUnexpectedSessionAck, wantCommitted: 4,
			acks: []handlers.SessionAck{{Action: "update", Generation: 7}}},
		{name: "start ack for an update", wantErr: ErrUnexpectedSessionAck, wantCommitted: 4,
			acks: []handlers.SessionAck{{Action: "start", Generation: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := &mockHandler{waitForCapabilities: true, sessionAcks: tc.acks}
			var state committedSessionState
			state.commit(startSpecInitial, 4)

			// The update on top of generation 5, the number the timed-out
			// update was sent with, is numbered 6.
			err := applySessionSpecWithGeneration("srv1", handler, &state, startSpecReloaded, 5, false,
				0, logging.NopLogger{})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("update error = %v, want %v", err, tc.wantErr)
			}
			if len(handler.commands) != 1 || !strings.HasPrefix(handler.commands[0], "SESSION UPDATE ") {
				t.Fatalf("commands = %q, want one SESSION UPDATE", handler.commands)
			}
			wantSpec := startSpecInitial
			if tc.wantErr == nil {
				wantSpec = startSpecReloaded
			}
			spec, generation, committed := state.snapshot()
			if !committed || generation != tc.wantCommitted || !reflect.DeepEqual(spec, wantSpec) {
				t.Fatalf("committed = %#v at %d (%v), want %#v at %d", spec, generation, committed,
					wantSpec, tc.wantCommitted)
			}
		})
	}
}
