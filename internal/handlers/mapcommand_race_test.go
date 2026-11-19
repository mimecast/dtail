package handlers

// TestAggregatePointerRaceWithShutdown is a negative test that exercises
// the race between handleMapCommand writing h.aggregate and baseHandler
// Shutdown reading that same pointer concurrently. Running this test with -race
// detects unsynchronized access before the atomic.Pointer fix is applied, and
// passes cleanly after. The regular-aggregate counterpart was removed with the
// regular mapaggregate.Aggregate itself (task hv0); output is now the only mapaggregate.
//
// The test does not set up a real MapReduce query; instead it injects a real
// output aggregate via the atomic accessors so the test is self-contained.

import (
	"context"
	"sync"
	"testing"

	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

func TestAggregatePointerRaceWithShutdown(t *testing.T) {

	const iterations = 200

	for i := 0; i < iterations; i++ {
		h := newBaseHandler(context.Background(), baseHandlerConfig{
			serverMessages: make(chan string, 8),
			maprMessages:   make(chan string, 4),
			user:           &userserver.User{Name: "race-test-output-user"},
		})

		ta, err := newHandlerTestAggregate("select count($0) from .", "")
		if err != nil {
			t.Skipf("could not create output aggregate: %v", err)
		}

		var wg sync.WaitGroup

		// Writer goroutine – simulates handleMapCommand setting the output mapaggregate.
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.setAggregate(ta)
		}()

		// Reader goroutine – simulates Aggregate() / Shutdown reading.
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.getAggregate()
		}()

		wg.Wait()
		// Cleanup: abort the output aggregate so its internal goroutines exit.
		if got := h.getAggregate(); got != nil {
			got.Abort()
		}
	}
}
