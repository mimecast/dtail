package mapr

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// TestGroupSetAvgZeroSamplesDoesNotProduceNaN is a negative test that reproduces
// the bug where an empty aggregate set (Samples==0) causes 0/0 = NaN in the Avg
// case of resultSelect. This happens when the server creates a group-set entry
// via GetSet before confirming that any select fields matched, then serialises and
// sends the empty set to the client. The client-side resultSelect must guard the
// Avg division so that Samples==0 yields 0 instead of NaN.
func TestGroupSetAvgZeroSamplesDoesNotProduceNaN(t *testing.T) {
	t.Parallel()

	query, err := NewQuery("select avg(latency) from stats group by host", logging.NopLogger{})
	if err != nil {
		t.Fatalf("Unable to parse query: %v", err)
	}

	groupSet := NewGroupSet(logging.NopLogger{})

	// Simulate what the server does when no log line fields match the select
	// clause: GetSet creates the entry, but Samples stays 0 and FValues is
	// never populated. This is the bug trigger — previously 0/0 = NaN.

	_ = groupSet.GetSet("host-a")

	rows, _, err := groupSet.result(query, false)
	if err != nil {
		t.Fatalf("result() returned unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Expected 1 result row (even for empty set), got %d", len(rows))
	}

	// Before the fix each floating-point value in the row was the string "NaN".
	for _, row := range rows {
		for _, v := range row.values {
			trimmed := strings.TrimSpace(v)
			f, parseErr := strconv.ParseFloat(trimmed, 64)
			if parseErr != nil {
				// Non-numeric values (e.g. integer count or last-string fields)
				// are fine; only floating-point results can be NaN.
				continue
			}
			if math.IsNaN(f) {
				t.Errorf("avg on empty set produced NaN in output %q; expected 0", v)
			}
		}
	}
}

// TestGroupSetAvgZeroSamplesResultOutputContainsNoNaN verifies that the
// higher-level Result method (which drives terminal output) also never emits
// "NaN" strings when aggregate sets have zero samples.
func TestGroupSetAvgZeroSamplesResultOutputContainsNoNaN(t *testing.T) {
	t.Parallel()

	query, err := NewQuery("select avg(latency) from stats group by host", logging.NopLogger{})
	if err != nil {
		t.Fatalf("Unable to parse query: %v", err)
	}

	groupSet := NewGroupSet(logging.NopLogger{})
	// Empty set — Samples==0, no FValues populated.

	_ = groupSet.GetSet("host-a")

	output, _, err := groupSet.Result(query, 100, nil)
	if err != nil {
		t.Fatalf("Result() returned unexpected error: %v", err)
	}

	if strings.Contains(output, "NaN") {
		t.Errorf("Result output must not contain 'NaN', got:\n%s", output)
	}
}
