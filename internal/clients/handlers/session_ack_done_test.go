package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
)

// TestWaitForSessionAckQueuedAckWinsOverDone: a server may acknowledge a
// SESSION request and end its session in one write, so the acknowledgement
// is queued and the handler done by the time the wait's select runs. With
// both cases ready, select picks one at random; the done case must still
// hand out the queued acknowledgement instead of reporting none (seen as a
// rare "timed out waiting for session acknowledgement" of a reload whose
// server had answered). Without a queued acknowledgement a done handler
// ends the wait at once.
func TestWaitForSessionAckQueuedAckWinsOverDone(t *testing.T) {
	// Many rounds: each wait must pick the done case at least sometimes.
	for range 200 {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		type result struct {
			ack SessionAck
			ok  bool
		}
		waited := make(chan result, 1)
		go func() {
			ack, ok := handler.waitForSessionAckIdle(context.Background(), time.Minute, 0)
			waited <- result{ack, ok}
		}()
		// Queue the acknowledgement and end the session together, so that
		// the waiting select sees both ready.
		handler.sessionAcks <- SessionAck{Action: "update", Generation: 2}
		handler.Shutdown()
		got := <-waited
		if !got.ok || got.ack.Action != "update" || got.ack.Generation != 2 {
			t.Fatalf("wait = %#v (%v), want the queued acknowledgement", got.ack, got.ok)
		}
	}

	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	handler.Shutdown()
	start := time.Now()
	if ack, ok := handler.waitForSessionAckIdle(context.Background(), time.Minute, 0); ok {
		t.Fatalf("wait on a done handler without an acknowledgement = %#v, want none", ack)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait on a done handler took %v", elapsed)
	}
}

// TestWaitForSessionAckContextEndsWithContext: the end of the context ends
// the wait at once (a :quit while a SESSION START is unacknowledged), but
// an acknowledgement queued by then still wins, as with a done handler.
func TestWaitForSessionAckContextEndsWithContext(t *testing.T) {
	for range 200 {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		ctx, cancel := context.WithCancel(context.Background())
		waited := make(chan bool, 1)
		go func() {
			_, ok := handler.waitForSessionAckIdle(ctx, time.Minute, 0)
			waited <- ok
		}()
		handler.sessionAcks <- SessionAck{Action: "start", Generation: 1}
		cancel()
		if !<-waited {
			t.Fatal("the queued acknowledgement lost against the ended context")
		}
		handler.Shutdown()
	}

	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	defer handler.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	if ack, ok := handler.WaitForSessionAckContext(ctx, time.Minute, 0); ok {
		t.Fatalf("wait without an acknowledgement = %#v, want none", ack)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait took %v after its context ended", elapsed)
	}
}
