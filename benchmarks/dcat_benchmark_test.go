package benchmarks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkDCatSimple benchmarks simple file reading
func BenchmarkDCatSimple(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size=%s", size), func(b *testing.B) {
			// Generate test file
			config := TestDataConfig{
				Size:          size,
				Format:        SimpleLogFormat,
				Compression:   NoCompression,
				LineVariation: 50,
			}

			testFile := GenerateTestFile(b, config)
			cleanupBenchmarkFile(b, testFile)

			fileSize := mustGetFileSize(b, testFile)
			lineCount := mustCountFileLines(b, testFile)

			// Warmup
			WarmupCommand(b, "dcat", "--plain", "--cfg", "none", testFile)

			b.ResetTimer()

			// Run benchmark
			totalDuration := time.Duration(0)
			for i := 0; i < b.N; i++ {
				result, err := RunBenchmarkCommand(b, "dcat", "--plain", "--cfg", "none", testFile)
				if err != nil {
					b.Fatalf("Command failed: %v", err)
				}
				totalDuration += result.Duration
			}

			avgDuration := totalDuration / time.Duration(b.N)
			throughput := CalculateThroughput(fileSize, avgDuration)
			linesPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

			// Report metrics
			b.ReportMetric(throughput, "MB/sec")
			b.ReportMetric(linesPerSec, "lines/sec")

			// Save result
			benchResult := BenchmarkResult{
				Timestamp:   time.Now(),
				Tool:        "dcat",
				Operation:   fmt.Sprintf("Simple_%s", size),
				FileSize:    fileSize,
				Duration:    avgDuration,
				Throughput:  throughput,
				LinesPerSec: linesPerSec,
			}
			saveBenchmarkResults(b, []BenchmarkResult{benchResult})
		})
	}
}

// BenchmarkDCatMultipleFiles benchmarks reading multiple files
func BenchmarkDCatMultipleFiles(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	numFiles := []int{10, 50, 100}
	fileSize := Small / 10 // 1MB each

	for _, num := range numFiles {
		b.Run(fmt.Sprintf("Files=%d", num), func(b *testing.B) {
			// Generate test files
			var testFiles []string
			totalSize := int64(0)
			totalLines := 0

			for i := 0; i < num; i++ {
				config := TestDataConfig{
					Size:          FileSize(fileSize),
					Format:        SimpleLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				testFiles = append(testFiles, testFile)
				cleanupBenchmarkFile(b, testFile)

				size := mustGetFileSize(b, testFile)
				lines := mustCountFileLines(b, testFile)
				totalSize += size
				totalLines += lines
			}

			// Warmup
			args := append([]string{"--plain", "--cfg", "none"}, testFiles...)
			WarmupCommand(b, "dcat", args...)

			b.ResetTimer()

			// Run benchmark
			totalDuration := time.Duration(0)
			for i := 0; i < b.N; i++ {
				result, err := RunBenchmarkCommand(b, "dcat", args...)
				if err != nil {
					b.Fatalf("Command failed: %v", err)
				}
				totalDuration += result.Duration
			}

			avgDuration := totalDuration / time.Duration(b.N)
			throughput := CalculateThroughput(totalSize, avgDuration)
			linesPerSec := CalculateLinesPerSecond(totalLines, avgDuration)

			// Report metrics
			b.ReportMetric(throughput, "MB/sec")
			b.ReportMetric(linesPerSec, "lines/sec")
			b.ReportMetric(float64(num), "files")

			// Save result
			benchResult := BenchmarkResult{
				Timestamp:   time.Now(),
				Tool:        "dcat",
				Operation:   fmt.Sprintf("MultiFile_%d", num),
				FileSize:    totalSize,
				Duration:    avgDuration,
				Throughput:  throughput,
				LinesPerSec: linesPerSec,
			}
			saveBenchmarkResults(b, []BenchmarkResult{benchResult})
		})
	}
}

// BenchmarkDCatCompressed benchmarks reading compressed files
func BenchmarkDCatCompressed(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	compressions := []struct {
		name string
		typ  CompressionType
	}{
		{"none", NoCompression},
		{"gzip", GzipCompression},
		{"zstd", ZstdCompression},
	}

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	for _, size := range sizes {
		for _, comp := range compressions {
			b.Run(fmt.Sprintf("Size=%s/Compression=%s", size, comp.name), func(b *testing.B) {
				// Generate test file
				config := TestDataConfig{
					Size:          size,
					Format:        SimpleLogFormat,
					Compression:   comp.typ,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				// Get uncompressed size for throughput calculation
				uncompressedSize := int64(size)
				compressedSize := mustGetFileSize(b, testFile)
				compressionRatio := float64(uncompressedSize) / float64(compressedSize)

				// Estimate line count (compressed files are harder to count)
				approxLineCount := int(size) / 150

				// Warmup
				WarmupCommand(b, "dcat", "--plain", "--cfg", "none", testFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)
				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dcat", "--plain", "--cfg", "none", testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
				}

				avgDuration := totalDuration / time.Duration(b.N)
				// Throughput based on uncompressed size
				throughput := CalculateThroughput(uncompressedSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(approxLineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")
				b.ReportMetric(compressionRatio, "compression_ratio")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dcat",
					Operation:   fmt.Sprintf("Compressed_%s_%s", comp.name, size),
					FileSize:    uncompressedSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: linesPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}

// BenchmarkDCatServerMode benchmarks server mode vs serverless
func BenchmarkDCatServerMode(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	// Skip if dserver binary doesn't exist
	dserverPath := filepath.Join("..", "dserver")
	if _, err := os.Stat(dserverPath); err != nil {
		b.Skip("dserver binary not found, skipping server mode benchmarks")
	}

	modes := []struct {
		name   string
		server bool
	}{
		{"serverless", false},
		{"server", true},
	}

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	for _, size := range sizes {
		for _, mode := range modes {
			b.Run(fmt.Sprintf("Size=%s/Mode=%s", size, mode.name), func(b *testing.B) {
				// Generate test file
				config := TestDataConfig{
					Size:          size,
					Format:        SimpleLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				lineCount := mustCountFileLines(b, testFile)

				var args []string

				if mode.server {
					// Start dserver
					// Note: In a real implementation, we'd need to:
					// 1. Start dserver in background
					// 2. Wait for it to be ready
					// 3. Run dcat with --servers flag
					// 4. Stop dserver after benchmark
					// For now, we'll skip the actual server mode implementation
					b.Skip("Server mode benchmarking requires additional setup")
				} else {
					args = []string{"--plain", "--cfg", "none", testFile}
				}

				// Warmup
				WarmupCommand(b, "dcat", args...)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)
				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dcat", args...)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dcat",
					Operation:   fmt.Sprintf("%s_%s", mode.name, size),
					FileSize:    fileSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: linesPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}
