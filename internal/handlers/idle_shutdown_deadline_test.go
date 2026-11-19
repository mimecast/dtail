package handlers

import (
	"context"
	"testing"
	"time"
)

// A command that ran into its own deadline ("timeout N ...") leaves an
// expired context behind. The idle shutdown after it must not run under that
// context: its close handshake would be skipped and the session closed at
// once. It runs under the connection's context instead, sends the close
// frame and waits for the client's acknowledgement.
func TestIdleShutdownAfterExpiredCommandDeadlineStillDoesTheCloseHandshake(t *testing.T) {
	handler := &ServerHandler{baseHandler: newReadTestHandler(), readTimings: newReadTimings(nil)}
	handler.ackCloseReceived = make(chan struct{})
	handler.commandWg.Add(1)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		handler.idleShutdownAfterCommand(ctx)
		close(shutdownDone)
	}()

	select {
	case message := <-handler.serverMessages:
		if message != ".syn close connection" {
			t.Fatalf("first message of the shutdown = %q, want the close frame", message)
		}
	case <-time.After(time.Second):
		t.Fatal("the shutdown sent no close frame")
	}
	select {
	case <-shutdownDone:
		t.Fatal("the shutdown did not wait for the close acknowledgement")
	case <-time.After(100 * time.Millisecond):
	}

	close(handler.ackCloseReceived)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("the shutdown did not finish after the close acknowledgement")
	}
}
