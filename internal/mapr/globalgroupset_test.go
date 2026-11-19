package mapr

import (
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

// TestUpdateNoblockSemaphoreReleasedOnPanic verifies that UpdateNoblock
// releases the semaphore even when its callback panics (e.g. due to a nil
// GroupSet). Without the fix (using defer), the semaphore would be leaked and
// subsequent calls like NumSets would deadlock forever.
func TestUpdateNoblockSemaphoreReleasedOnPanic(t *testing.T) {
	g := NewGlobalGroupSet(logging.NopLogger{})

	// Calling GetSet on a nil *GroupSet in the callback causes a nil-pointer
	// dereference. We catch the panic in a goroutine and verify that the
	// GlobalGroupSet is still usable afterwards.

	done := make(chan struct{})
	go func() {
		defer func() {
			// Recover the expected panic so the goroutine exits cleanly.
			if r := recover(); r == nil {
				t.Errorf("expected a panic from UpdateNoblock with nil GroupSet, got none")
			}
			close(done)
		}()
		// This must panic internally; with the bug the semaphore is never released.
		g.UpdateNoblock(func(*GroupSet) {
			var nilGroup *GroupSet
			nilGroup.GetSet("key")
		})
	}()

	// Wait for the goroutine to finish (panic recovered).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for UpdateNoblock panic to be recovered")
	}

	// After the panic the semaphore must have been released by the deferred
	// release in UpdateNoblock. If the bug is present NumSets acquires the same
	// 1-slot semaphore and blocks forever, causing the test to time out.
	result := make(chan int, 1)
	go func() {
		result <- g.NumSets()
	}()

	select {
	case n := <-result:
		if n != 0 {
			t.Errorf("expected 0 sets in empty GlobalGroupSet, got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NumSets deadlocked: semaphore was not released after UpdateNoblock panic (bug reproduced)")
	}
}

// TestUpdateNoblockNormalOperation verifies the non-panic happy path still
// works correctly: an update with a free semaphore returns true and NumSets
// reflects the merged data, and an update while the semaphore is held returns
// false without calling its callback.
func TestUpdateNoblockNormalOperation(t *testing.T) {
	g := NewGlobalGroupSet(logging.NopLogger{})

	// One aggregate set, so there is something to merge.
	set := NewAggregateSet()
	set.FValues["count"] = 1

	// A minimal query is enough; the merge only needs query.Select, which can
	// be empty for this structural test (no select conditions to iterate).
	query := &Query{}

	var err error
	merged := g.UpdateNoblock(func(dst *GroupSet) { err = dst.GetSet("key1").Merge(query, set) })
	if err != nil {
		t.Errorf("unexpected error from the merge: %v", err)
	}
	if !merged {
		t.Error("expected UpdateNoblock to return true when semaphore is free")
	}
	g.Update(func(*GroupSet) {
		if g.UpdateNoblock(func(*GroupSet) { t.Error("UpdateNoblock ran while the semaphore was held") }) {
			t.Error("expected UpdateNoblock to return false while the semaphore is held")
		}
	})

	// After merging, NumSets must return 1 and must not deadlock.
	result := make(chan int, 1)
	go func() {
		result <- g.NumSets()
	}()

	select {
	case n := <-result:
		if n != 1 {
			t.Errorf("expected 1 set after merge, got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NumSets deadlocked after normal UpdateNoblock (unexpected)")
	}
}
