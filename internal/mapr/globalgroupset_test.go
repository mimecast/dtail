package mapr

import (
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

// TestMergeNoblockSemaphoreReleasedOnPanic verifies that MergeNoblock releases
// the semaphore even when g.merge panics (e.g. due to a nil GroupSet).
// Without the fix (using defer), the semaphore would be leaked and subsequent
// calls like NumSets would deadlock forever.
func TestMergeNoblockSemaphoreReleasedOnPanic(t *testing.T) {
	g := NewGlobalGroupSet(logging.NopLogger{})

	// Calling MergeNoblock with a nil *GroupSet causes a nil-pointer dereference
	// inside g.merge when it iterates over group.sets. We catch the panic in a
	// goroutine and verify that the GlobalGroupSet is still usable afterwards.

	done := make(chan struct{})
	go func() {
		defer func() {
			// Recover the expected panic so the goroutine exits cleanly.
			if r := recover(); r == nil {
				t.Errorf("expected a panic from MergeNoblock with nil GroupSet, got none")
			}
			close(done)
		}()
		// This must panic internally; with the bug the semaphore is never released.
		//nolint:staticcheck // intentional nil dereference to exercise the panic path
		_, _ = g.MergeNoblock(nil, nil) //nolint:errcheck
	}()

	// Wait for the goroutine to finish (panic recovered).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for MergeNoblock panic to be recovered")
	}

	// After the panic the semaphore must have been released by the deferred
	// release in MergeNoblock. If the bug is present NumSets acquires the same
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
		t.Fatal("NumSets deadlocked: semaphore was not released after MergeNoblock panic (bug reproduced)")
	}
}

// TestMergeNoblockNormalOperation verifies the non-panic happy path still works
// correctly: a successful merge returns (true, nil) and NumSets reflects the
// merged data.
func TestMergeNoblockNormalOperation(t *testing.T) {
	g := NewGlobalGroupSet(logging.NopLogger{})
	group := NewGroupSet(logging.NopLogger{})

	// Populate the group set with one entry so there is something to merge.

	set := NewAggregateSet()
	set.FValues["count"] = 1
	group.sets["key1"] = set

	// A minimal query is enough; the merge loop only needs query.Select which
	// can be empty for this structural test (no select conditions to iterate).
	query := &Query{}

	merged, err := g.MergeNoblock(query, group)
	if err != nil {
		t.Errorf("unexpected error from MergeNoblock: %v", err)
	}
	if !merged {
		t.Error("expected MergeNoblock to return merged=true when semaphore is free")
	}

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
		t.Fatal("NumSets deadlocked after normal MergeNoblock (unexpected)")
	}
}
