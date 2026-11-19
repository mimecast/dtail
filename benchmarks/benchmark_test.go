package benchmarks

import (
	"fmt"
	"os"
	"testing"
)

// TestMain sets up and tears down the benchmark environment
func TestMain(m *testing.M) {
	// Clean up any leftover files before starting
	if err := CleanupBenchmarkFiles(""); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to cleanup old files: %v\n", err)
	}

	// Run tests/benchmarks
	code := m.Run()

	// Clean up after benchmarks unless asked to keep files
	if os.Getenv("DTAIL_BENCH_KEEP_FILES") != "true" {
		if err := CleanupBenchmarkFiles(""); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup files: %v\n", err)
		}
	}

	os.Exit(code)
}

// BenchmarkAll runs a representative subset of all benchmarks
func BenchmarkAll(b *testing.B) {
	b.Run("DCat", func(b *testing.B) {
		BenchmarkDCatSimple(b)
	})

	b.Run("DGrep", func(b *testing.B) {
		BenchmarkDGrepSimplePattern(b)
	})

	b.Run("DMap", func(b *testing.B) {
		BenchmarkDMapSimpleAggregation(b)
	})
}

// BenchmarkQuick runs only quick benchmarks with small files
func BenchmarkQuick(b *testing.B) {
	// Set quick mode
	b.Setenv("DTAIL_BENCH_QUICK", "true")

	// Set small files only
	b.Setenv("DTAIL_BENCH_SIZES", "small")

	BenchmarkAll(b)
}
