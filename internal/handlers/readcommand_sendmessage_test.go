package handlers

// Tests for readCommand.sendServerMessage.
//
// Leak context: the session's serverMessages channel (capacity 10 in the real
// ServerHandler) is drained only by baseHandler.Read. Once the client
// disconnects and the session shuts down, Read stops draining, so a bare
// blocking send would pin its goroutine forever. The warn paths hit this in
// practice: readGlob warns once per retry and readFileIfPermissions warns once
// per permission-denied file, and a single glob may fan out to up to
// MaxGlobTargets goroutines. sendServerMessage must therefore abandon the send
// when the per-command context is cancelled (the context is cancelled on
// command completion and handler shutdown via the handler-owned command root).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
)

// newSendMessageTestCommand returns a readCommand whose server messages
// channel has the given capacity, so tests can control exactly when the
// channel is full.
func newSendMessageTestCommand(channelCapacity int) (*readCommand, *globCapTestServer) {
	srv := newGlobCapTestServer(10)
	srv.serverMessage = make(chan string, channelCapacity)
	return newReadCommand(srv, omode.CatClient), srv
}

// fillServerMessagesChannel fills the channel to capacity so any further
// send would block.
func fillServerMessagesChannel(t *testing.T, ch chan string) {
	t.Helper()
	for i := 0; i < cap(ch); i++ {
		select {
		case ch <- fmt.Sprintf("filler %d", i):
		default:
			t.Fatalf("channel unexpectedly full while pre-filling (i=%d, cap=%d)", i, cap(ch))
		}
	}
}

// TestSendServerMessageReturnsOnCancelledContext verifies the core leak fix:
// with a full channel and an already-cancelled context, sendServerMessage must
// return instead of blocking forever — and the abandoned message must never
// surface on the channel afterwards.
func TestSendServerMessageReturnsOnCancelledContext(t *testing.T) {

	cmd, srv := newSendMessageTestCommand(2)
	fillServerMessagesChannel(t, srv.serverMessage)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	returned := make(chan struct{})
	go func() {
		cmd.sendServerMessage(ctx, "must not block")
		close(returned)
	}()

	select {
	case <-returned:
		// Fixed behavior: the send was abandoned on ctx.Done().
	case <-time.After(2 * time.Second):
		t.Fatal("sendServerMessage blocked on a full channel despite cancelled context (goroutine leak)")
	}

	// An abandoned send must be dropped for good: draining the channel now
	// must yield only the filler messages, never the abandoned one.
	drained := 0
	for {
		select {
		case raw := <-srv.serverMessage:
			if !strings.HasPrefix(raw, "filler ") {
				t.Fatalf("abandoned message leaked onto the channel: %q", raw)
			}
			drained++
		default:
			if want := cap(srv.serverMessage); drained != want {
				t.Fatalf("expected %d filler messages on the channel, drained %d", want, drained)
			}
			return
		}
	}
}

// TestSendServerMessageUnblocksExactlyOnCancel verifies the send stays pending
// while the context is live (no message is dropped prematurely) and is
// abandoned as soon as the context is cancelled.
func TestSendServerMessageUnblocksExactlyOnCancel(t *testing.T) {

	cmd, srv := newSendMessageTestCommand(1)
	fillServerMessagesChannel(t, srv.serverMessage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		close(started)
		cmd.sendServerMessage(ctx, "pending message")
		close(returned)
	}()

	// Wait until the sender goroutine has been scheduled before starting the
	// "still pending" window, so the check below cannot pass vacuously just
	// because the goroutine never ran.
	<-started

	// While ctx is live and the channel is full, the send must still be
	// pending — sendServerMessage must not silently drop the message.
	select {
	case <-returned:
		t.Fatal("sendServerMessage returned although channel is full and context is live")
	case <-time.After(100 * time.Millisecond):
		// Still pending, as expected.
	}

	cancel()

	select {
	case <-returned:
		// Abandoned on cancel, as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("sendServerMessage did not return after context cancellation")
	}
}

// TestSendServerMessageDeliversWhenChannelHasRoom verifies the happy path:
// with room in the channel and a live context, the message is delivered with
// the command's generation encoded and a trailing newline appended.
func TestSendServerMessageDeliversWhenChannelHasRoom(t *testing.T) {

	cmd, srv := newSendMessageTestCommand(2)
	cmd.generation = 7

	cmd.sendServerMessage(context.Background(), "hello")

	select {
	case raw := <-srv.serverMessage:
		generation, message := decodeGeneratedMessage(raw)
		if generation != 7 {
			t.Errorf("expected generation 7, got %d", generation)
		}
		if message != "hello\n" {
			t.Errorf("expected message %q, got %q", "hello\n", message)
		}
	default:
		t.Fatal("expected a message on the server messages channel, got none")
	}
}

// TestSendServerMessageDeliversToDrainedChannel verifies that a send blocked
// on a full channel completes normally (message delivered, not abandoned)
// once a consumer drains the channel while the context stays live. This
// mirrors the healthy-session case where baseHandler.Read keeps draining.
func TestSendServerMessageDeliversToDrainedChannel(t *testing.T) {

	cmd, srv := newSendMessageTestCommand(1)
	fillServerMessagesChannel(t, srv.serverMessage)

	returned := make(chan struct{})
	go func() {
		cmd.sendServerMessage(context.Background(), "queued")
		close(returned)
	}()

	// Drain the filler message; the pending send must now complete.
	<-srv.serverMessage

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("sendServerMessage did not complete after the channel was drained")
	}

	select {
	case raw := <-srv.serverMessage:
		if _, message := decodeGeneratedMessage(raw); message != "queued\n" {
			t.Errorf("expected message %q, got %q", "queued\n", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the queued message to be delivered")
	}
}

// TestSendServerMessageReleasedByHandlerShutdown exercises the production
// wiring end-to-end: the per-command context comes from the handler's
// connection-derived command root. A warn-sender stuck on a full
// serverMessages channel must be released directly by handler shutdown.
func TestSendServerMessageReleasedByHandlerShutdown(t *testing.T) {

	handler := newBaseHandler(context.Background(), baseHandlerConfig{
		serverMessages: make(chan string, 1),
	})

	// Wire the readCommand's server messages channel to the real handler's
	// channel so the stuck send targets the same channel baseHandler.Read
	// would drain in production.
	srv := newGlobCapTestServer(10)
	srv.serverMessage = handler.serverMessages
	cmd := newReadCommand(srv, omode.CatClient)

	fillServerMessagesChannel(t, handler.serverMessages)

	ctx, cancel := handler.newCommandContext()
	defer cancel()

	started := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		close(started)
		cmd.sendServerMessage(ctx, "stuck warn message")
		close(returned)
	}()

	// Ensure the sender is scheduled and pending before shutting down, so the
	// release below is attributable to done.Shutdown() rather than the send
	// never having started.
	<-started
	select {
	case <-returned:
		t.Fatal("sendServerMessage returned although channel is full and handler is not shut down")
	case <-time.After(100 * time.Millisecond):
		// Still pending, as expected.
	}

	// Simulate session teardown after a client disconnect while nothing drains
	// serverMessages.
	handler.Shutdown()

	select {
	case <-returned:
		// Released via the handler-owned connection-derived command context.
	case <-time.After(2 * time.Second):
		t.Fatal("sendServerMessage not released by handler shutdown (goroutine leak)")
	}
}
