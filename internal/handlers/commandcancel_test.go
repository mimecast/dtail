package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestHandleCommandCancelsContextAfterCommandFinished verifies that
// baseHandler.handleCommand no longer discards the cancel func returned by
// newCommandContext. The cancel must fire when commandFinished is invoked.
func TestHandleCommandCancelsContextAfterCommandFinished(t *testing.T) {

	handler := newSessionTestHandler("handle-command-cancel-user")
	readServerMessage(t, handler.serverMessages)
	handler.handleCommandCb = handler.handleUserCommand

	type captured struct {
		ctx    context.Context
		finish func()
	}
	ch := make(chan captured, 1)
	handler.commands = map[string]commandHandler{
		// AUTHKEY is a side-effect command so commandFinished does not
		// trigger handler shutdown, keeping the test focused on the
		// per-command cancel contract.
		"AUTHKEY": func(ctx context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
			ch <- captured{ctx: ctx, finish: commandFinished}
		},
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("AUTHKEY dummy"))
	handler.handleCommand("protocol " + protocol.ProtocolCompat + " base64 " + encoded)

	var got captured
	select {
	case got = <-ch:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AUTHKEY command was not dispatched")
	}

	select {
	case <-got.ctx.Done():
		t.Fatal("per-command context cancelled before commandFinished ran")
	default:
	}

	got.finish()

	select {
	case <-got.ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("per-command context was not cancelled after commandFinished ran; cancel was discarded")
	}
}

// TestNewCommandContextDoesNotSpawnWatcherGoroutine proves command context
// construction has constant goroutine cost. Cancellation flows through the
// handler-owned parent context instead of a goroutine watching a done channel.
func TestNewCommandContextDoesNotSpawnWatcherGoroutine(t *testing.T) {
	h := newBaseHandler(context.Background(), baseHandlerConfig{})
	t.Cleanup(h.Shutdown)

	baseline := runtime.NumGoroutine()

	const N = 100
	ctxs := make([]context.Context, 0, N)
	for i := 0; i < N; i++ {
		ctx, _ := h.newCommandContext()
		ctxs = append(ctxs, ctx)
	}

	if delta := runtime.NumGoroutine() - baseline; delta > 4 {
		t.Fatalf("creating %d command contexts spawned goroutines: delta=%d", N, delta)
	}

	h.cancelCommands()

	deadline := time.Now().Add(time.Second)
	for i, ctx := range ctxs {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("context %d not cancelled by handler shutdown", i)
		}
		select {
		case <-ctx.Done():
		case <-time.After(remaining):
			t.Fatalf("context %d not cancelled by handler shutdown", i)
		}
	}

}

func TestConnectionCancellationStopsBlockedCommand(t *testing.T) {
	connCtx, cancelConnection := context.WithCancel(context.Background())
	h := newBaseHandler(connCtx, baseHandlerConfig{})
	t.Cleanup(h.Shutdown)
	started := make(chan context.Context, 1)
	stopped := make(chan struct{})
	h.handleCommandCb = func(ctx context.Context, _ lcontext.LContext, _ int, _ []string, _ string) {
		started <- ctx
		<-ctx.Done()
		close(stopped)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("BLOCK"))
	go h.handleCommand("protocol " + protocol.ProtocolCompat + " base64 " + encoded)
	var commandCtx context.Context
	select {
	case commandCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}

	cancelConnection()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("blocked command outlived its connection context")
	}
	if !errors.Is(commandCtx.Err(), context.Canceled) {
		t.Fatalf("command context error = %v, want context.Canceled", commandCtx.Err())
	}
}
