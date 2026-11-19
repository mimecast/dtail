package handlers

import (
	"context"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

// A client may number an update like the workload already running (its
// rollback after an acknowledgement timeout does). The acknowledgement
// carries the client's number, but the server's own generation still moves
// on, so that output queued by the replaced workload is stale.
func TestSessionUpdateWithRepeatedClientGenerationGetsNewServerGeneration(t *testing.T) {
	handler, recorder := newSessionDispatchTestHandler("session-repeated-generation-user")
	readServerMessage(t, handler.serverMessages)
	t.Cleanup(func() {
		if handler.sessionState.cancel != nil {
			handler.sessionState.cancel()
		}
		recorder.wg.Wait()
	})
	payload := func(file string) string {
		return mustSessionPayload(t, session.Spec{Mode: omode.TailClient, Files: []string{file}, Regex: "ERROR"})
	}

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", payload("/var/log/a.log")}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	recorder.waitForStart(t)
	seen := map[uint64]bool{handler.sessionState.currentGeneration(): true}

	for _, file := range []string{"/var/log/b.log", "/var/log/c.log"} {
		handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 4,
			[]string{"SESSION", "UPDATE", "2", payload(file)}, func() {})
		if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 2" {
			t.Fatalf("unexpected session update ack: %q", message)
		}
		recorder.waitForStart(t)
		generation := handler.sessionState.currentGeneration()
		if seen[generation] {
			t.Fatalf("server generation %d was reused for %s", generation, file)
		}
		seen[generation] = true
	}
}

// The command names are matched exactly, like the command table: a
// mixed-case name is an unknown command, which ends the session.
func TestShouldShutdownOnCommandCompletionMatchesNamesExactly(t *testing.T) {
	for name, want := range map[string]bool{
		"AUTHKEY": false, "SESSION": false,
		"AuthKey": true, "Session": true, "cat": true, "nosuch": true,
	} {
		if got := shouldShutdownOnCommandCompletion(name); got != want {
			t.Errorf("shouldShutdownOnCommandCompletion(%q) = %v, want %v", name, got, want)
		}
	}
}
