package mapr

import (
	"sync"
	"testing"
)

// TestSafeAggregateSetConcurrency tests that SafeAggregateSet handles concurrent operations correctly.
func TestSafeAggregateSetConcurrency(t *testing.T) {
	safeSet := NewSafeAggregateSet()

	// Number of concurrent goroutines
	numGoroutines := 100
	// Number of operations per goroutine
	opsPerGoroutine := 1000

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Launch concurrent goroutines
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()

			for j := 0; j < opsPerGoroutine; j++ {
				// Test Count operation
				err := safeSet.Aggregate("count", Count, "1", false)
				if err != nil {
					t.Errorf("Error in Count aggregation: %v", err)
				}

				// Test Sum operation
				err = safeSet.Aggregate("sum", Sum, "10.5", false)
				if err != nil {
					t.Errorf("Error in Sum aggregation: %v", err)
				}

				// Test Last operation
				err = safeSet.Aggregate("last", Last, "value", false)
				if err != nil {
					t.Errorf("Error in Last aggregation: %v", err)
				}

				// Test Min operation
				err = safeSet.Aggregate("min", Min, "5.0", false)
				if err != nil {
					t.Errorf("Error in Min aggregation: %v", err)
				}

				// Test Max operation
				err = safeSet.Aggregate("max", Max, "15.0", false)
				if err != nil {
					t.Errorf("Error in Max aggregation: %v", err)
				}

				// Increment samples
				safeSet.IncrementSamples()
			}
		}()
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify results
	clone := safeSet.Clone()

	// Check Count
	expectedCount := float64(numGoroutines * opsPerGoroutine)
	if clone.FValues["count"] != expectedCount {
		t.Errorf("Expected count %f, got %f", expectedCount, clone.FValues["count"])
	}

	// Check Sum
	expectedSum := float64(numGoroutines*opsPerGoroutine) * 10.5
	if clone.FValues["sum"] != expectedSum {
		t.Errorf("Expected sum %f, got %f", expectedSum, clone.FValues["sum"])
	}

	// Check Min
	if clone.FValues["min"] != 5.0 {
		t.Errorf("Expected min 5.0, got %f", clone.FValues["min"])
	}

	// Check Max
	if clone.FValues["max"] != 15.0 {
		t.Errorf("Expected max 15.0, got %f", clone.FValues["max"])
	}

	// Check Samples
	expectedSamples := numGoroutines * opsPerGoroutine
	if clone.Samples != expectedSamples {
		t.Errorf("Expected samples %d, got %d", expectedSamples, clone.Samples)
	}

	// Check Last (should be "value")
	if clone.SValues["last"] != "value" {
		t.Errorf("Expected last 'value', got '%s'", clone.SValues["last"])
	}
}

// TestSafeAggregateSetClone tests that cloning creates an independent copy.
func TestSafeAggregateSetClone(t *testing.T) {
	original := NewSafeAggregateSet()

	// Add some data
	if err := original.Aggregate("count", Count, "1", false); err != nil {
		t.Fatalf("aggregate count: %v", err)
	}
	if err := original.Aggregate("sum", Sum, "100", false); err != nil {
		t.Fatalf("aggregate sum: %v", err)
	}
	if err := original.Aggregate("last", Last, "original", false); err != nil {
		t.Fatalf("aggregate last: %v", err)
	}
	original.IncrementSamples()

	// Clone the set
	clone := original.Clone()

	// Modify the original
	if err := original.Aggregate("count", Count, "1", false); err != nil {
		t.Fatalf("aggregate modified count: %v", err)
	}
	if err := original.Aggregate("sum", Sum, "50", false); err != nil {
		t.Fatalf("aggregate modified sum: %v", err)
	}
	if err := original.Aggregate("last", Last, "modified", false); err != nil {
		t.Fatalf("aggregate modified last: %v", err)
	}
	original.IncrementSamples()

	// Verify clone is unchanged
	if clone.FValues["count"] != 1 {
		t.Errorf("Clone count should be 1, got %f", clone.FValues["count"])
	}

	if clone.FValues["sum"] != 100 {
		t.Errorf("Clone sum should be 100, got %f", clone.FValues["sum"])
	}

	if clone.SValues["last"] != "original" {
		t.Errorf("Clone last should be 'original', got '%s'", clone.SValues["last"])
	}

	if clone.Samples != 1 {
		t.Errorf("Clone samples should be 1, got %d", clone.Samples)
	}

	// Verify original has changed
	originalClone := original.Clone()
	if originalClone.FValues["count"] != 2 {
		t.Errorf("Original count should be 2, got %f", originalClone.FValues["count"])
	}

	if originalClone.FValues["sum"] != 150 {
		t.Errorf("Original sum should be 150, got %f", originalClone.FValues["sum"])
	}
}
