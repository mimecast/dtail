package handlers

// TestReadSemaphoreNotStolenOnCancelBeforeAcquire is a negative test
// (regression guard) for the semaphore-slot-stealing bug.
//
// Root cause: the original defer unconditionally performed a non-blocking
// receive from the limiter channel regardless of whether this goroutine had
// actually acquired a slot. When a goroutine was cancelled by ctx.Done()
// before it sent to the limiter, the defer still ran and drained one slot
// that belonged to a different goroutine. The real holder's own defer would
// then drain yet another slot, permanently reducing the semaphore capacity.
//
// Fix: track a local `acquired bool` flag; only drain the slot in the defer
// when acquired == true.
//
// The test fills the limiter to capacity, then spins up N goroutines that
// all call read() with a pre-cancelled context. None of them should ever
// acquire a slot, so after all goroutines exit the limiter must still hold
// exactly `cap(limiter)` items. Before the fix each goroutine would steal
// one slot, draining the semaphore below capacity and preventing legitimate
// future acquires.

import (
	"context"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// buildLimiterTestHandler returns a minimal ServerHandler with the catLimiter
// pre-filled to its capacity so that every subsequent send blocks.
func buildLimiterTestHandler(t *testing.T, capacity int) (*ServerHandler, chan struct{}) {
	t.Helper()

	limiter := make(chan struct{}, capacity)
	for i := 0; i < capacity; i++ {
		// Fill the semaphore so it appears fully occupied.
		limiter <- struct{}{}
	}

	handler := &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{
			logger:         handlerTestLogger,
			serverMessages: make(chan string, 64),
			maprMessages:   make(chan string, 4),
			user:           &userserver.User{Name: "semaphore-test-user"},
		}),
		serverCfg:   &config.ServerConfig{},
		catLimiter:  limiter,
		tailLimiter: limiter,
	}
	// activeGeneration must be set so generation-bound writers match the
	// production constructor's session state.
	handler.activeGeneration = handler.sessionState.currentGeneration

	return handler, limiter
}

// TestReadSemaphoreNotStolenOnCancelBeforeAcquire verifies that goroutines
// cancelled before they acquire a semaphore slot do not decrement the slot
// count. With the bug present, each of the N goroutines would drain one slot
// via the unconditional defer, reducing limiter length to zero.
func TestReadSemaphoreNotStolenOnCancelBeforeAcquire(t *testing.T) {

	const (
		capacity = 5
		workers  = 20 // more workers than capacity to force blocking
	)

	handler, limiter := buildLimiterTestHandler(t, capacity)

	// Pre-cancel the context so every read() call hits ctx.Done() before it
	// can ever send to the full limiter and returns without acquiring a slot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			cmd := newReadCommand(handler, omode.CatClient)
			// Pass nil target so read() uses the non-validated snapshot path.
			cmd.read(ctx, lcontext.LContext{}, "/nonexistent/file.log", nil, "test-glob", regex.NewNoop())
		}()
	}
	wg.Wait()

	// After all goroutines that were cancelled before acquiring a slot have
	// exited, the limiter must still hold exactly `capacity` items. Any count
	// lower than capacity means slots were stolen from legitimate holders.
	got := len(limiter)
	if got != capacity {
		t.Fatalf("semaphore capacity corrupted: want %d slots, got %d slots after %d cancelled goroutines",
			capacity, got, workers)
	}
}
