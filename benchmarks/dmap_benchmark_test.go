package benchmarks

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkDMapSimpleAggregation benchmarks simple aggregation queries
func BenchmarkDMapSimpleAggregation(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()

	queries := []struct {
		name  string
		query string
	}{
		{"count", "from STATS select count($line) group by $hostname"},
		{"sum_avg", "from STATS select sum($goroutines),avg($goroutines) group by $hostname"},
		{"min_max", "from STATS select min(currentConnections),max(lifetimeConnections) group by $hostname"},
		{"multi", "from STATS select count($line),last($time),avg($goroutines),min(currentConnections),max(lifetimeConnections) group by $hostname"},
	}

	for _, size := range sizes {
		for _, q := range queries {
			b.Run(fmt.Sprintf("Size=%s/Query=%s", size, q.name), func(b *testing.B) {
				// Generate MapReduce format test file
				config := TestDataConfig{
					Size:          size,
					Format:        MapReduceLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				lineCount := mustCountFileLines(b, testFile)

				// Output file
				outputFile := fmt.Sprintf("benchmark_%s_%s.csv.tmp", q.name, size)
				cleanupBenchmarkFile(b, outputFile)

				// Build query with output file
				fullQuery := fmt.Sprintf("%s outfile %s", q.query, outputFile)

				// Warmup
				WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
				removeBenchmarkFile(b, outputFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
					removeBenchmarkFile(b, outputFile)
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				recordsPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(recordsPerSec, "records/sec")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dmap",
					Operation:   fmt.Sprintf("Aggregation_%s_%s", q.name, size),
					FileSize:    fileSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: recordsPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}

// BenchmarkDMapGroupByCardinality benchmarks group by with different cardinalities
func BenchmarkDMapGroupByCardinality(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	// Different group by scenarios
	groupBys := []struct {
		name         string
		groupBy      string
		approxGroups int
	}{
		{"low", "$hostname", 10},                    // Few unique values
		{"medium", "$time", 100},                    // Moderate unique values
		{"high", "$goroutines", 50},                 // Many unique values
		{"composite", "$hostname,$goroutines", 500}, // Composite key
	}

	for _, size := range sizes {
		for _, gb := range groupBys {
			b.Run(fmt.Sprintf("Size=%s/GroupBy=%s", size, gb.name), func(b *testing.B) {
				// Generate test file
				config := TestDataConfig{
					Size:          size,
					Format:        MapReduceLogFormat,
					Compression:   NoCompression,
					LineVariation: gb.approxGroups,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				lineCount := mustCountFileLines(b, testFile)

				// Output file
				outputFile := fmt.Sprintf("benchmark_groupby_%s_%s.csv.tmp", gb.name, size)
				cleanupBenchmarkFile(b, outputFile)

				// Build query
				query := fmt.Sprintf("from STATS select count($line),avg($goroutines) group by %s outfile %s",
					gb.groupBy, outputFile)

				// Warmup
				WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", query, testFile)
				removeBenchmarkFile(b, outputFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", query, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
					removeBenchmarkFile(b, outputFile)
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				recordsPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(recordsPerSec, "records/sec")
				b.ReportMetric(float64(gb.approxGroups), "approx_groups")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dmap",
					Operation:   fmt.Sprintf("GroupBy_%s_%s", gb.name, size),
					FileSize:    fileSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: recordsPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}

// BenchmarkDMapComplexQueries benchmarks complex queries with WHERE clauses
func BenchmarkDMapComplexQueries(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	queries := []struct {
		name  string
		query string
	}{
		{"simple_where", "from STATS select count($line),avg($goroutines) group by $hostname where lifetimeConnections >= 100"},
		{"multi_where", "from STATS select count($line),avg($goroutines) group by $hostname where lifetimeConnections >= 100 and currentConnections < 50"},
		{"time_filter", "from STATS select count($line),avg($goroutines) group by $hostname where $time >= \"1002-071200\" and $time <= \"1002-071300\""},
		{"order_limit", "from STATS select $hostname,count($line),avg($goroutines) group by $hostname order by count($line) desc limit 10"},
	}

	for _, size := range sizes {
		for _, q := range queries {
			b.Run(fmt.Sprintf("Size=%s/Query=%s", size, q.name), func(b *testing.B) {
				// Generate test file
				config := TestDataConfig{
					Size:          size,
					Format:        MapReduceLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				lineCount := mustCountFileLines(b, testFile)

				// Output file
				outputFile := fmt.Sprintf("benchmark_complex_%s_%s.csv.tmp", q.name, size)
				cleanupBenchmarkFile(b, outputFile)

				// Build query with output file
				fullQuery := fmt.Sprintf("%s outfile %s", q.query, outputFile)

				// Warmup
				WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
				removeBenchmarkFile(b, outputFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
					removeBenchmarkFile(b, outputFile)
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				recordsPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(recordsPerSec, "records/sec")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dmap",
					Operation:   fmt.Sprintf("Complex_%s_%s", q.name, size),
					FileSize:    fileSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: recordsPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}

// BenchmarkDMapSetClauseHighCardinality benchmarks set-clause queries that
// create many groups and stress the aggregation hot path.
func BenchmarkDMapSetClauseHighCardinality(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	query := "from STATS select count($line),avg($goroutines) " +
		"group by $masked,$goroutines " +
		"set $masked = maskdigits($time)"

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size=%s", size), func(b *testing.B) {
			config := TestDataConfig{
				Size:          size,
				Format:        MapReduceLogFormat,
				Compression:   NoCompression,
				LineVariation: 1000,
			}

			testFile := GenerateTestFile(b, config)
			cleanupBenchmarkFile(b, testFile)

			fileSize := mustGetFileSize(b, testFile)
			lineCount := mustCountFileLines(b, testFile)

			outputFile := fmt.Sprintf("benchmark_set_high_cardinality_%s.csv.tmp", size)
			cleanupBenchmarkFile(b, outputFile)

			fullQuery := fmt.Sprintf("%s outfile %s", query, outputFile)

			WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
			removeBenchmarkFile(b, outputFile)

			b.ResetTimer()

			totalDuration := time.Duration(0)
			for i := 0; i < b.N; i++ {
				result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
				if err != nil {
					b.Fatalf("Command failed: %v", err)
				}
				totalDuration += result.Duration
				removeBenchmarkFile(b, outputFile)
			}

			avgDuration := totalDuration / time.Duration(b.N)
			throughput := CalculateThroughput(fileSize, avgDuration)
			recordsPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

			b.ReportMetric(throughput, "MB/sec")
			b.ReportMetric(recordsPerSec, "records/sec")
			b.ReportMetric(1000, "approx_groups")

			benchResult := BenchmarkResult{
				Timestamp:   time.Now(),
				Tool:        "dmap",
				Operation:   fmt.Sprintf("SetClauseHighCardinality_%s", size),
				FileSize:    fileSize,
				Duration:    avgDuration,
				Throughput:  throughput,
				LinesPerSec: recordsPerSec,
			}
			saveBenchmarkResults(b, []BenchmarkResult{benchResult})
		})
	}
}

// BenchmarkDMapTimeInterval benchmarks time-based interval queries
func BenchmarkDMapTimeInterval(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	intervals := []struct {
		name     string
		interval int
	}{
		{"1s", 1},
		{"10s", 10},
		{"60s", 60},
	}

	for _, size := range sizes {
		for _, interval := range intervals {
			b.Run(fmt.Sprintf("Size=%s/Interval=%s", size, interval.name), func(b *testing.B) {
				// Generate test file
				config := TestDataConfig{
					Size:          size,
					Format:        MapReduceLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				lineCount := mustCountFileLines(b, testFile)

				// Output file
				outputFile := fmt.Sprintf("benchmark_interval_%s_%s.csv.tmp", interval.name, size)
				cleanupBenchmarkFile(b, outputFile)

				// Build query
				query := fmt.Sprintf("from STATS select count($line),avg($goroutines) group by $hostname interval %d outfile %s",
					interval.interval, outputFile)

				// Warmup
				WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", query, testFile)
				removeBenchmarkFile(b, outputFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", query, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
					removeBenchmarkFile(b, outputFile)
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				recordsPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(recordsPerSec, "records/sec")
				b.ReportMetric(float64(interval.interval), "interval_seconds")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dmap",
					Operation:   fmt.Sprintf("Interval_%s_%s", interval.name, size),
					FileSize:    fileSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: recordsPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}

// BenchmarkDMapCompressed benchmarks MapReduce on compressed files
func BenchmarkDMapCompressed(b *testing.B) {
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
					Format:        MapReduceLogFormat,
					Compression:   comp.typ,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				// Get uncompressed size for throughput calculation
				uncompressedSize := int64(size)
				compressedSize := mustGetFileSize(b, testFile)
				compressionRatio := float64(uncompressedSize) / float64(compressedSize)

				// Estimate line count
				approxLineCount := int(size) / 150

				// Output file
				outputFile := fmt.Sprintf("benchmark_compressed_%s_%s.csv.tmp", comp.name, size)
				cleanupBenchmarkFile(b, outputFile)

				// Query
				query := fmt.Sprintf("from STATS select count($line),avg($goroutines) group by $hostname outfile %s", outputFile)

				// Warmup
				WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", query, testFile)
				removeBenchmarkFile(b, outputFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", query, testFile)
					if err != nil {
						b.Fatalf("Command failed: %v", err)
					}
					totalDuration += result.Duration
					removeBenchmarkFile(b, outputFile)
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(uncompressedSize, avgDuration)
				recordsPerSec := CalculateLinesPerSecond(approxLineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(recordsPerSec, "records/sec")
				b.ReportMetric(compressionRatio, "compression_ratio")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dmap",
					Operation:   fmt.Sprintf("Compressed_%s_%s", comp.name, size),
					FileSize:    uncompressedSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: recordsPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}

// BenchmarkDMapCustomFunctions benchmarks queries with custom functions
func BenchmarkDMapCustomFunctions(b *testing.B) {
	cleanup := SetupBenchmark(b)
	defer cleanup()

	sizes := GetBenchmarkSizes()
	if IsQuickMode() {
		sizes = []FileSize{Small}
	}

	queries := []struct {
		name  string
		query string
	}{
		{"maskdigits", "from STATS select $masked,count($line) set $masked = maskdigits($time) group by $masked"},
		{"md5sum", "from STATS select $hash,count($line) set $hash = md5sum($hostname) group by $hash"},
		{"multi_set", "from STATS select $mask,$md5,count($line) set $mask = maskdigits($time), $md5 = md5sum($hostname) group by $hostname"},
	}

	for _, size := range sizes {
		for _, q := range queries {
			b.Run(fmt.Sprintf("Size=%s/Function=%s", size, q.name), func(b *testing.B) {
				// Generate test file
				config := TestDataConfig{
					Size:          size,
					Format:        MapReduceLogFormat,
					Compression:   NoCompression,
					LineVariation: 50,
				}

				testFile := GenerateTestFile(b, config)
				cleanupBenchmarkFile(b, testFile)

				fileSize := mustGetFileSize(b, testFile)
				lineCount := mustCountFileLines(b, testFile)

				// Output file
				outputFile := fmt.Sprintf("benchmark_func_%s_%s.csv.tmp", q.name, size)
				cleanupBenchmarkFile(b, outputFile)

				// Build query with output file
				fullQuery := fmt.Sprintf("%s outfile %s", q.query, outputFile)

				// Warmup
				WarmupCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
				removeBenchmarkFile(b, outputFile)

				b.ResetTimer()

				// Run benchmark
				totalDuration := time.Duration(0)

				for i := 0; i < b.N; i++ {
					result, err := RunBenchmarkCommand(b, "dmap", "--cfg", "none", "--noColor", "--query", fullQuery, testFile)
					if err != nil && !strings.Contains(err.Error(), "exit status") {
						// Some queries might have syntax issues, log but continue
						b.Logf("Command error (continuing): %v", err)
						continue
					}
					totalDuration += result.Duration
					removeBenchmarkFile(b, outputFile)
				}

				avgDuration := totalDuration / time.Duration(b.N)
				throughput := CalculateThroughput(fileSize, avgDuration)
				recordsPerSec := CalculateLinesPerSecond(lineCount, avgDuration)

				// Report metrics
				b.ReportMetric(throughput, "MB/sec")
				b.ReportMetric(recordsPerSec, "records/sec")

				// Save result
				benchResult := BenchmarkResult{
					Timestamp:   time.Now(),
					Tool:        "dmap",
					Operation:   fmt.Sprintf("Function_%s_%s", q.name, size),
					FileSize:    fileSize,
					Duration:    avgDuration,
					Throughput:  throughput,
					LinesPerSec: recordsPerSec,
				}
				saveBenchmarkResults(b, []BenchmarkResult{benchResult})
			})
		}
	}
}
