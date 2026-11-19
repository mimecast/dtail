package benchmarks

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkDGrepSimplePattern benchmarks simple string matching
func BenchmarkDGrepSimplePattern(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	hitRates := []int{1, 10, 50, 90} // Percentage of lines matching

	for _, size := range sizes {
		for _, hitRate := range hitRates {
			b.Run(fmt.Sprintf("Size=%s/HitRate=%d%%", size, hitRate), func(b *testing.B) {
				// Generate test file with pattern
				pattern := "ERROR"
				config := TestDataConfig{
					Size:          size,
					Format:        SimpleLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
					Pattern:       pattern,
					PatternRate:   hitRate,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				totalLines := mustCountFileLines(b, testFile)

				// Warmup
				WarmupCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pattern, testFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)
				matchedLines := 0

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pattern, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration

					// Count matched lines (only once)
					if i == 0 {
						matchedLines = len(strings.Split(strings.TrimSpace(result.Stdout), "\n"))
						if result.Stdout == "" {
							matchedLines = 0
						}
					}
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(totalLines, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")
				b.ReportMetric(float64(matchedLines), "matched_lines")
				b.ReportMetric(float64(hitRate), "hit_rate_%")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dgrep",
					Operation:   fmt.Sprintf("Simple_%s_HR%d", size, hitRate),
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

// BenchmarkDGrepRegexPattern benchmarks complex regex patterns
func BenchmarkDGrepRegexPattern(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	patterns := []struct {
		name    string
		pattern string
	}{
		{"simple", "ERROR.*failed"},
		{"complex", "\\d{4}-\\d{6}.*ERROR.*connection.*[0-9]+"},
		{"alternation", "(ERROR|WARN|FATAL)"},
		{"capture", "thread-(\\d+).*line:(\\d+)"},
	}

	for _, size := range sizes {
		for _, pat := range patterns {
			b.Run(fmt.Sprintf("Size=%s/Pattern=%s", size, pat.name), func(b *testing.B) {
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
				totalLines := mustCountFileLines(b, testFile)

				// Warmup
				WarmupCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pat.pattern, testFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pat.pattern, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(totalLines, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dgrep",
					Operation:   fmt.Sprintf("Regex_%s_%s", pat.name, size),
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

// BenchmarkDGrepContext benchmarks grep with context lines
func BenchmarkDGrepContext(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	contexts := []struct {
		name   string
		before int
		after  int
	}{
		{"none", 0, 0},
		{"small", 2, 2},
		{"medium", 5, 5},
		{"large", 10, 10},
	}

	for _, size := range sizes {
		for _, ctx := range contexts {
			b.Run(fmt.Sprintf("Size=%s/Context=%s", size, ctx.name), func(b *testing.B) {
				// Generate test file
				pattern := "ERROR"
				config := TestDataConfig{
					Size:          size,
					Format:        SimpleLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
					Pattern:       pattern,
					PatternRate:   10, // 10% hit rate
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				totalLines := mustCountFileLines(b, testFile)

				// Build command args
				args := []string{"--plain", "--cfg", "none", "--grep", pattern}
				if ctx.before > 0 {
					args = append(args, "--before", fmt.Sprintf("%d", ctx.before))
				}
				if ctx.after > 0 {
					args = append(args, "--after", fmt.Sprintf("%d", ctx.after))
				}
				args = append(args, testFile)

				// Warmup
				WarmupCommand(b, "dgrep", args...)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dgrep", args...)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(totalLines, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")
				b.ReportMetric(float64(ctx.before+ctx.after), "context_lines")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dgrep",
					Operation:   fmt.Sprintf("Context_%s_%s", ctx.name, size),
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

// BenchmarkDGrepInvert benchmarks inverted grep
func BenchmarkDGrepInvert(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()

	// Test with different exclusion rates
	exclusionRates := []int{10, 50, 90} // Percentage of lines to exclude

	for _, size := range sizes {
		for _, excludeRate := range exclusionRates {
			b.Run(fmt.Sprintf("Size=%s/ExcludeRate=%d%%", size, excludeRate), func(b *testing.B) {
				// Generate test file
				pattern := "EXCLUDE"
				config := TestDataConfig{
					Size:          size,
					Format:        SimpleLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
					Pattern:       pattern,
					PatternRate:   excludeRate,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				totalLines := mustCountFileLines(b, testFile)

				// Warmup
				WarmupCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pattern, "--invert", testFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pattern, "--invert", testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(totalLines, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")
				b.ReportMetric(float64(100-excludeRate), "output_rate_%")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dgrep",
					Operation:   fmt.Sprintf("Invert_%s_ER%d", size, excludeRate),
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

// BenchmarkDGrepCompressed benchmarks grep on compressed files
func BenchmarkDGrepCompressed(b *testing.B) {
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
				pattern := "ERROR"
				config := TestDataConfig{
					Size:          size,
					Format:        SimpleLogFormat,
					Compression:   comp.typ,
					LineVariation: 50,
					Pattern:       pattern,
					PatternRate:   10,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				// Get uncompressed size for throughput calculation
				uncompressedSize := int64(size)
				compressedSize := mustGetFileSize(b, testFile)
				compressionRatio := float64(uncompressedSize) / float64(compressedSize)

				// Estimate line count
				approxLineCount := int(size) / 150

				// Warmup
				WarmupCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pattern, testFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dgrep", "--plain", "--cfg", "none", "--grep", pattern, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(uncompressedSize, avgDuration)
				linesPerSec := CalculateLinesPerSecond(approxLineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(linesPerSec, "lines/sec")
				b.ReportMetric(compressionRatio, "compression_ratio")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dgrep",
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
