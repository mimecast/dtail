package benchmarks

import (
	"testing"
)

// BenchmarkDGrepMultipleFiles tests buffer pooling effectiveness with multiple files
func BenchmarkDGrepMultipleFiles(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	// Create multiple test files
	numFiles := 10
	files := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		config := TestDataConfig{
			Size:          Small,
			Format:        SimpleLogFormat,
			Compression:   NoCompression,
			LineVariation: 50,
			Pattern:       "ERROR",
			PatternRate:   10,
		}
		files[i] = GenerateTestFile(b, config)
		cleanupBenchmarkFile(b, files[i])
	}

	b.Run("WithTurbo", func(b *testing.B) {
		// Turbo boost is enabled by default; clear the disable flag so this
		// "WithTurbo" arm genuinely runs with turbo boost on even if a prior
		// benchmark left DTAIL_TURBOBOOST_DISABLE set in the process env.
		b.Setenv("DTAIL_TURBOBOOST_DISABLE", "")

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			// Process all files
			for _, file := range files {
				_, err := RunBenchmarkCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", "ERROR", file)
				if err != nil {
					b.Fatalf("Failed to run dgrep: %v", err)
				}
			}
		}
	})
}

// BenchmarkDGrepLargeFile tests performance on a single large file
func BenchmarkDGrepLargeFile(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	config := TestDataConfig{
		Size:          Medium,
		Format:        SimpleLogFormat,
		Compression:   NoCompression,
		LineVariation: 50,
		Pattern:       "ERROR",
		PatternRate:   10,
	}

	testFile := GenerateTestFile(b, config)
	cleanupBenchmarkFile(b, testFile)

	b.Run("WithTurbo", func(b *testing.B) {
		// Turbo boost is enabled by default; clear the disable flag so this
		// "WithTurbo" arm genuinely runs with turbo boost on even if a prior
		// benchmark left DTAIL_TURBOBOOST_DISABLE set in the process env.
		b.Setenv("DTAIL_TURBOBOOST_DISABLE", "")

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			result, err := RunBenchmarkCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", "ERROR", testFile)
			if err != nil {
				b.Fatalf("Failed to run dgrep: %v", err)
			}
			_ = result
		}
	})
}
