package mapr

import (
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// TestGroupSetResultOrderIsDeterministicWithoutOrderBy is a negative test that
// reproduces the non-determinism bug in result(): when OrderBy is unset, the
// output row order depended on Go's map iteration order, which is intentionally
// randomised per runtime invocation. Two consecutive calls to result() on the
// same GroupSet could return rows in different orders.
//
// The fix collects group keys, sorts them lexicographically before building
// rows, and only then applies SortStable for the OrderBy pass. Ties on OrderBy
// (or no OrderBy) therefore resolve to lexicographic groupKey order rather than
// to random map iteration order.
func TestGroupSetResultOrderIsDeterministicWithoutOrderBy(t *testing.T) {
	t.Parallel()

	// Query with no ORDER BY clause — the bug case where map iteration order
	// was the sole determinant of row order.
	query, err := NewQuery("select count(line) from logs group by host", logging.NopLogger{})
	if err != nil {
		t.Fatalf("Unable to parse query: %v", err)
	}

	groupSet := NewGroupSet(logging.NopLogger{})

	// Insert keys in reverse lexicographic order to ensure the expected sorted
	// order cannot coincide with insertion order.

	for _, host := range []string{"host-z", "host-m", "host-a", "host-b"} {
		set := groupSet.GetSet(host)
		if err := set.Aggregate("count(line)", Count, "1", false); err != nil {
			t.Fatalf("Aggregate failed for %s: %v", host, err)
		}
	}

	// Run result() many times. Before the fix a handful of iterations was
	// enough to observe a different ordering; with the fix every call must
	// return exactly the same lexicographically sorted sequence of groupKeys.
	var firstKeys []string
	const iterations = 50
	for i := range iterations {
		rows, _, err := groupSet.result(query, false)
		if err != nil {
			t.Fatalf("result() iteration %d returned error: %v", i, err)
		}
		if len(rows) != 4 {
			t.Fatalf("Expected 4 rows, got %d on iteration %d", len(rows), i)
		}

		keys := make([]string, len(rows))
		for j, r := range rows {
			keys[j] = r.groupKey
		}

		if i == 0 {
			firstKeys = keys
			// Verify the order is lexicographic (the contract of the fix).
			expected := []string{"host-a", "host-b", "host-m", "host-z"}
			if !reflect.DeepEqual(keys, expected) {
				t.Fatalf("First result not in lexicographic order: got %v, want %v", keys, expected)
			}
			continue
		}

		// Every subsequent call must return the identical key sequence.
		if !reflect.DeepEqual(keys, firstKeys) {
			t.Fatalf("Non-deterministic ordering detected on iteration %d: got %v, want %v", i, keys, firstKeys)
		}
	}
}

// TestGroupSetResultOrderIsDeterministicWithOrderByTies verifies that when
// multiple rows share the same OrderBy value (a tie), the tie-break falls back
// to lexicographic groupKey order rather than to random map iteration order.
// SortStable preserves the relative order of equal elements, so the pre-sort of
// keys guarantees a deterministic tie-break.
func TestGroupSetResultOrderIsDeterministicWithOrderByTies(t *testing.T) {
	t.Parallel()

	// ORDER BY count(line) — all rows will have the same count (1), creating a
	// full tie that must resolve to lexicographic groupKey order.
	query, err := NewQuery("select count(line) from logs group by host order by count(line)", logging.NopLogger{})
	if err != nil {
		t.Fatalf("Unable to parse query: %v", err)
	}

	groupSet := NewGroupSet(logging.NopLogger{})

	// All hosts receive the same count value to force a tie.

	for _, host := range []string{"host-z", "host-m", "host-a", "host-b"} {
		set := groupSet.GetSet(host)
		if err := set.Aggregate("count(line)", Count, "1", false); err != nil {
			t.Fatalf("Aggregate failed for %s: %v", host, err)
		}
	}

	var firstKeys []string
	const iterations = 50
	for i := range iterations {
		rows, _, err := groupSet.result(query, false)
		if err != nil {
			t.Fatalf("result() iteration %d returned error: %v", i, err)
		}
		if len(rows) != 4 {
			t.Fatalf("Expected 4 rows, got %d on iteration %d", len(rows), i)
		}

		keys := make([]string, len(rows))
		for j, r := range rows {
			keys[j] = r.groupKey
		}

		if i == 0 {
			firstKeys = keys
			continue
		}

		if !reflect.DeepEqual(keys, firstKeys) {
			t.Fatalf("Non-deterministic tie-break detected on iteration %d: got %v, want %v", i, keys, firstKeys)
		}
	}
}
