package benchmarks

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// BenchmarkDCatDirect benchmarks dcat in direct/serverless mode
func BenchmarkDCatDirect(b *testing.B) {
	// Create test file if it doesn't exist
	testFile := filepath.Join(b.TempDir(), "benchmark_test.log")

	// Create a 100K line file for benchmarking
	var buf bytes.Buffer
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&buf, "2025-01-01 10:00:00 INFO [app] Processing request ID=%d status=OK latency=42ms user=user%d path=/api/v1/endpoint%d\n",
			i, i%100, i%10)
	}

	if err := os.WriteFile(testFile, buf.Bytes(), 0644); err != nil {
		b.Fatal(err)
	}

	// Ensure dcat binary exists
	dcatPath := "../dcat"
	if _, err := os.Stat(dcatPath); err != nil {
		b.Skip("dcat binary not found, run 'make build' first")
	}

	// Turbo boost is enabled by default and disabled via DTAIL_TURBOBOOST_DISABLE.
	// The non-turbo arm sets DTAIL_TURBOBOOST_DISABLE=yes; the turbo arm unsets it
	// so the two arms genuinely exercise the two code paths.
	b.Run("NonTurbo", func(b *testing.B) {
		b.Setenv("DTAIL_TURBOBOOST_DISABLE", "yes")
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			cmd := exec.Command(dcatPath, testFile)
			cmd.Stdout = io.Discard
			if err := cmd.Run(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Turbo", func(b *testing.B) {
		b.Setenv("DTAIL_TURBOBOOST_DISABLE", "")
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			cmd := exec.Command(dcatPath, testFile)
			cmd.Stdout = io.Discard
			if err := cmd.Run(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkDCatDirectWithSizes tests different file sizes
func BenchmarkDCatDirectWithSizes(b *testing.B) {
	sizes := []struct {
		name  string
		lines int
	}{
		{"1K", 1000},
		{"10K", 10000},
		{"100K", 100000},
	}

	dcatPath := "../dcat"
	if _, err := os.Stat(dcatPath); err != nil {
		b.Skip("dcat binary not found, run 'make build' first")
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			// Create test file
			testFile := filepath.Join(b.TempDir(), fmt.Sprintf("benchmark_%s.log", size.name))
			var buf bytes.Buffer
			for i := 0; i < size.lines; i++ {
				fmt.Fprintf(&buf, "2025-01-01 10:00:00 INFO [app] Line %d data\n", i)
			}
			if err := os.WriteFile(testFile, buf.Bytes(), 0644); err != nil {
				b.Fatal(err)
			}

			// Non-turbo arm disables turbo boost; turbo arm relies on the
			// default-on behaviour by clearing DTAIL_TURBOBOOST_DISABLE.
			b.Run("NonTurbo", func(b *testing.B) {
				b.Setenv("DTAIL_TURBOBOOST_DISABLE", "yes")
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					cmd := exec.Command(dcatPath, testFile)
					cmd.Stdout = io.Discard
					if err := cmd.Run(); err != nil {
						b.Fatal(err)
					}
				}
			})

			b.Run("Turbo", func(b *testing.B) {
				b.Setenv("DTAIL_TURBOBOOST_DISABLE", "")
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					cmd := exec.Command(dcatPath, testFile)
					cmd.Stdout = io.Discard
					if err := cmd.Run(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
