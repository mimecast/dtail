package benchmarks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// BenchmarkResult captures performance metrics from a benchmark run
type BenchmarkResult struct {
	Timestamp   time.Time
	Tool        string // dcat, dgrep, dmap
	Operation   string // specific benchmark name
	FileSize    int64
	Duration    time.Duration
	Throughput  float64 // MB/sec
	LinesPerSec float64
	MemoryUsage int64
	CPUTime     time.Duration
	ExitCode    int
	Error       error
	GitCommit   string
	GoVersion   string
}

// CommandResult captures the output and metrics from running a command
type CommandResult struct {
	Stdout      string
	Stderr      string
	Duration    time.Duration
	ExitCode    int
	MemoryUsage int64
	Error       error
}

// RunBenchmarkCommand executes a DTail command and captures metrics
func RunBenchmarkCommand(b *testing.B, cmd string, args ...string) (*CommandResult, error) {
	b.Helper()

	// Look for command in parent directory (from benchmarks/ to ../)
	cmdPath := filepath.Join("..", cmd)
	if _, err := os.Stat(cmdPath); err != nil {
		return nil, fmt.Errorf("command %s not found: %w", cmdPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	command := exec.CommandContext(ctx, cmdPath, args...)

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	startTime := time.Now()
	err := command.Run()
	duration := time.Since(startTime)

	result := &CommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
		Error:    err,
	}

	exitErr := &exec.ExitError{}
	switch {
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	case err == nil:
		result.ExitCode = 0
	default:
		result.ExitCode = -1
	}

	// Note: Memory usage tracking would require platform-specific code
	// or running under a profiler. For now, we'll leave it as 0.
	result.MemoryUsage = 0

	return result, nil
}

// CalculateThroughput computes MB/sec from file size and duration
func CalculateThroughput(fileSize int64, duration time.Duration) float64 {
	if duration == 0 {
		return 0
	}
	megabytes := float64(fileSize) / (1024 * 1024)
	seconds := duration.Seconds()
	return megabytes / seconds
}

// CalculateLinesPerSecond computes lines/sec from line count and duration
func CalculateLinesPerSecond(lineCount int, duration time.Duration) float64 {
	if duration == 0 {
		return 0
	}
	return float64(lineCount) / duration.Seconds()
}

// CountFileLines counts the number of lines in a file
func CountFileLines(filename string) (int, error) {
	// Use wc -l equivalent for efficiency
	cmd := exec.Command("wc", "-l", filename)
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	var lines int
	n, err := fmt.Sscanf(string(output), "%d", &lines)
	if err != nil {
		return 0, fmt.Errorf("parse line count for %q: %w", filename, err)
	}
	if n != 1 {
		return 0, fmt.Errorf("parse line count for %q: got %d values", filename, n)
	}
	return lines, nil
}

// GetFileSize returns the size of a file in bytes
func GetFileSize(filename string) (int64, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// SetupBenchmark prepares the benchmark environment
func SetupBenchmark(b *testing.B) func() {
	b.Helper()

	// Store original working directory
	originalWd, err := os.Getwd()
	if err != nil {
		b.Fatalf("Failed to get working directory: %v", err)
	}

	// Ensure we're in the benchmarks directory
	if !strings.HasSuffix(originalWd, "benchmarks") {
		benchDir := filepath.Join(originalWd, "benchmarks")
		if err := os.Chdir(benchDir); err != nil {
			b.Fatalf("Failed to change to benchmarks directory: %v", err)
		}
	}

	// Clean up any leftover files
	if err := CleanupBenchmarkFiles(""); err != nil {
		b.Logf("Warning: failed to cleanup old files: %v", err)
	}

	// Return cleanup function
	return func() {
		// Clean up benchmark files
		if keepFiles := os.Getenv("DTAIL_BENCH_KEEP_FILES"); keepFiles != "true" {
			if err := CleanupBenchmarkFiles(""); err != nil {
				b.Logf("Warning: failed to cleanup files: %v", err)
			}
		}

		// Restore working directory
		if err := os.Chdir(originalWd); err != nil {
			b.Errorf("Failed to restore working directory: %v", err)
		}
	}
}

func mustGetFileSize(tb testing.TB, filename string) int64 {
	tb.Helper()
	size, err := GetFileSize(filename)
	if err != nil {
		tb.Fatalf("get file size for %s: %v", filename, err)
	}
	return size
}

func mustCountFileLines(tb testing.TB, filename string) int {
	tb.Helper()
	lines, err := CountFileLines(filename)
	if err != nil {
		tb.Fatalf("count lines in %s: %v", filename, err)
	}
	return lines
}

func saveBenchmarkResults(tb testing.TB, results []BenchmarkResult) {
	tb.Helper()
	if err := SaveResults(results); err != nil {
		tb.Fatalf("save benchmark results: %v", err)
	}
}

func cleanupBenchmarkFile(tb testing.TB, filename string) {
	tb.Helper()
	tb.Cleanup(func() { _ = os.Remove(filename) })
}

func removeBenchmarkFile(tb testing.TB, filename string) {
	tb.Helper()
	if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
		tb.Fatalf("remove benchmark file %s: %v", filename, err)
	}
}

// ReportBenchmarkMetrics adds custom metrics to benchmark results
func ReportBenchmarkMetrics(b *testing.B, result *BenchmarkResult) {
	b.Helper()

	if result.Throughput > 0 {
		b.ReportMetric(result.Throughput, "MB/sec")
	}

	if result.LinesPerSec > 0 {
		b.ReportMetric(result.LinesPerSec, "lines/sec")
	}

	if result.MemoryUsage > 0 {
		b.ReportMetric(float64(result.MemoryUsage)/(1024*1024), "MB_memory")
	}
}

// GetBenchmarkSizes returns the file sizes to test based on environment
func GetBenchmarkSizes() []FileSize {
	sizesEnv := os.Getenv("DTAIL_BENCH_SIZES")
	if sizesEnv == "" {
		// Default to all sizes
		return []FileSize{Small, Medium, Large}
	}

	var sizes []FileSize
	for _, sizeStr := range strings.Split(sizesEnv, ",") {
		switch strings.ToLower(strings.TrimSpace(sizeStr)) {
		case "small", "10mb":
			sizes = append(sizes, Small)
		case "medium", "100mb":
			sizes = append(sizes, Medium)
		case "large", "1gb":
			sizes = append(sizes, Large)
		}
	}

	if len(sizes) == 0 {
		// Fallback to small if nothing valid specified
		return []FileSize{Small}
	}

	return sizes
}

// IsQuickMode checks if we should run quick benchmarks only
func IsQuickMode() bool {
	return os.Getenv("DTAIL_BENCH_QUICK") == "true"
}

// GetBenchmarkTimeout returns the timeout for benchmark operations
func GetBenchmarkTimeout() time.Duration {
	timeoutStr := os.Getenv("DTAIL_BENCH_TIMEOUT")
	if timeoutStr == "" {
		return 30 * time.Minute
	}

	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return 30 * time.Minute
	}

	return timeout
}

// GetGitCommit returns the current git commit hash
func GetGitCommit() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

// GetGoVersion returns the Go version
func GetGoVersion() string {
	return runtime.Version()
}

// WarmupCommand runs a command once to warm up caches
func WarmupCommand(b *testing.B, cmd string, args ...string) {
	b.Helper()

	// Run once without timing
	_, err := RunBenchmarkCommand(b, cmd, args...)
	if err != nil {
		b.Logf("Warmup run failed (this may be expected): %v", err)
	}
}
