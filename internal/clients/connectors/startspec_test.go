package connectors

import (
	"errors"
	"testing"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

var (
	startSpecInitial  = sessionspec.Spec{Mode: omode.TailClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"}
	startSpecReloaded = sessionspec.Spec{Mode: omode.TailClient, Files: []string{"/var/log/app.log"}, Regex: "WARN"}
)

// startCommandOf returns the SESSION START command of spec.
func startCommandOf(t *testing.T, spec sessionspec.Spec) string {
	t.Helper()
	command, err := spec.StartCommand()
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	return command
}

// TestReplaceStartSessionSpecBeforeStart: a connection still connecting
// whose start spec a :reload replaced sends the reloaded spec in its
// initial SESSION START, not the spec it was created with, and commits it.
func TestReplaceStartSessionSpecBeforeStart(t *testing.T) {
	mock := &mockHandler{waitForCapabilities: true,
		sessionAcks: []handlers.SessionAck{{Action: "start", Generation: 1}}}
	conn := &ServerConnection{server: "srv1", handler: mock}

	if replaced, err := conn.ReplaceStartSessionSpec(startSpecReloaded); !replaced || err != nil {
		t.Fatalf("ReplaceStartSessionSpec() = %v, %v before the start, want true", replaced, err)
	}
	err := dispatchInitialCommands("srv1", mock, nil, true, startSpecInitial, &conn.sessionState, logging.NopLogger{})
	if err != nil {
		t.Fatalf("dispatchInitialCommands() error = %v", err)
	}
	if len(mock.commands) != 1 || mock.commands[0] != startCommandOf(t, startSpecReloaded) {
		t.Fatalf("sent %#v, want only the SESSION START of the reloaded spec", mock.commands)
	}
	if spec, generation, ok := conn.CommittedSession(); !ok || generation != 1 || spec.Regex != "WARN" {
		t.Fatalf("committed %#v generation %d ok %v, want the reloaded spec at 1", spec, generation, ok)
	}
}

// TestReplaceStartSessionSpecAfterStart: once the initial SESSION START
// began the start spec is not replaced, whether the start succeeded or
// failed, so the caller knows to update a committed session instead.
func TestReplaceStartSessionSpecAfterStart(t *testing.T) {
	for _, acked := range []bool{true, false} {
		mock := &mockHandler{waitForCapabilities: true}
		if acked {
			mock.sessionAcks = []handlers.SessionAck{{Action: "start", Generation: 1}}
		}
		conn := &Serverless{handler: mock}
		err := dispatchInitialCommands("srv1", mock, nil, true, startSpecInitial, &conn.sessionState, logging.NopLogger{})
		if acked != (err == nil) || (!acked && !errors.Is(err, ErrSessionAckTimeout)) {
			t.Fatalf("acked=%v: dispatchInitialCommands() error = %v", acked, err)
		}
		if replaced, replaceErr := conn.ReplaceStartSessionSpec(startSpecReloaded); replaced || replaceErr != nil {
			t.Fatalf("acked=%v: ReplaceStartSessionSpec() = %v, %v after the start began, want false, nil", acked, replaced, replaceErr)
		}
		if spec, _, ok := conn.CommittedSession(); ok != acked || (acked && spec.Regex != "ERROR") {
			t.Fatalf("acked=%v: committed %#v ok %v, want the initial spec only if acked", acked, spec, ok)
		}
	}
}
