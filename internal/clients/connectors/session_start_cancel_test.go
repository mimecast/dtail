package connectors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestSessionStartWaitEndsWithContext: a :quit or Ctrl-C (the connection's
// context ends) while the initial SESSION START is unacknowledged ends the
// start's wait at once with the context's error. It used to return only
// after defaultSessionAckTimeout (2 s) of idle handler time, as the wait
// watched the handler alone. The server here never acknowledges.
func TestSessionStartWaitEndsWithContext(t *testing.T) {
	handler := handlers.NewClientHandler("srv1", clientlog.NopLogger{})
	defer handler.Shutdown()
	served := serveSession(t, handler, "SESSION START ", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var state committedSessionState
	startErr := make(chan error, 1)
	go func() {
		startErr <- dispatchInitialCommands(ctx, "srv1", handler, nil, true, startSpecInitial,
			&state, logging.NopLogger{})
	}()
	<-served
	begin := time.Now()
	cancel()

	select {
	case err := <-startErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dispatchInitialCommands() error = %v, want context.Canceled", err)
		}
	case <-time.After(defaultSessionAckTimeout / 2):
		t.Fatalf("the start still waited %v after its context ended", time.Since(begin))
	}
	if _, _, committed := state.snapshot(); committed {
		t.Fatal("session committed without an acknowledgement")
	}
}

// TestSessionStartQueuedAckWinsOverContext: an acknowledgement that is
// already queued when the context ends is still taken, so a start the
// server acknowledged is committed and not reported as canceled.
func TestSessionStartQueuedAckWinsOverContext(t *testing.T) {
	handler := handlers.NewClientHandler("srv1", clientlog.NopLogger{})
	defer handler.Shutdown()
	served := serveSession(t, handler, "SESSION START ", protocol.HiddenSessionStartOKPrefix+" 1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	request, err := newSessionRequest(startSpecInitial, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = handler.SendMessage(request.command); err != nil {
		t.Fatal(err)
	}
	<-served
	ack, err := request.awaitAck(ctx, "srv1", handler, defaultSessionAckTimeout, 0, logging.NopLogger{})
	if err != nil || ack.Generation != 1 {
		t.Fatalf("awaitAck() = %+v, %v with the ack queued, want generation 1", ack, err)
	}
}

// plainAckHandler is a handler without WaitForSessionAckContext.
type plainAckHandler struct{ *mockHandler }

// TestWaitForSessionAckWithoutContextSupport: a handler that can not end
// its wait by a context is waited for as its own WaitForSessionAck bounds
// it, and a context that ended meanwhile is still what awaitAck reports.
func TestWaitForSessionAckWithoutContextSupport(t *testing.T) {
	handler := plainAckHandler{&mockHandler{waitForCapabilities: true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	request := sessionRequest{action: "start"}
	if _, err := request.awaitAck(ctx, "srv1", handler, time.Millisecond, 0, logging.NopLogger{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("awaitAck() error = %v, want context.Canceled", err)
	}
	if _, err := request.awaitAck(context.Background(), "srv1", handler, time.Millisecond, 0,
		logging.NopLogger{}); !errors.Is(err, ErrSessionAckTimeout) {
		t.Fatalf("awaitAck() error = %v without an acknowledgement, want ErrSessionAckTimeout", err)
	}
}
