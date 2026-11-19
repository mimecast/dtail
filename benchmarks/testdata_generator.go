package benchmarks

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DataDog/zstd"
)

// FileSize represents the size category of test files
type FileSize int

// Supported benchmark file sizes.
const (
	// Small represents a 10MB test file.
	Small  FileSize = 10 * 1024 * 1024   // 10MB
	Medium FileSize = 100 * 1024 * 1024  // 100MB
	Large  FileSize = 1024 * 1024 * 1024 // 1GB
)

func (fs FileSize) String() string {
	switch fs {
	case Small:
		return "10MB"
	case Medium:
		return "100MB"
	case Large:
		return "1GB"
	default:
		return fmt.Sprintf("%dB", fs)
	}
}

// LogFormat represents different log format types
type LogFormat int

// Supported synthetic log formats.
const (
	// SimpleLogFormat contains plain text log lines.
	SimpleLogFormat LogFormat = iota
	MapReduceLogFormat
	MixedLogFormat
)

// CompressionType represents file compression options
type CompressionType int

// Supported compression modes for generated test files.
const (
	// NoCompression stores test data without compression.
	NoCompression CompressionType = iota
	GzipCompression
	ZstdCompression
)

// TestDataConfig configures test data generation
type TestDataConfig struct {
	Size          FileSize
	Format        LogFormat
	Compression   CompressionType
	LineVariation int    // Percentage of unique lines (0-100)
	Pattern       string // Pattern to include for grep testing
	PatternRate   int    // Percentage of lines containing pattern (0-100)
}

// GenerateTestFile creates a test log file based on config
func GenerateTestFile(tb testing.TB, config TestDataConfig) string {
	tb.Helper()

	// Create temp file with .tmp suffix
	tmpFile, err := os.CreateTemp("", "dtail_bench_*.log.tmp")
	if err != nil {
		tb.Fatalf("Failed to create temp file: %v", err)
	}
	filename := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		if removeErr := os.Remove(filename); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, fmt.Errorf("remove unclosed temporary file: %w", removeErr))
		}
		tb.Fatalf("Failed to close temp file: %v", err)
	}

	// Apply compression if needed
	var finalFilename string
	switch config.Compression {
	case GzipCompression:
		finalFilename = filename + ".gz"
		if err := generateCompressedFile(filename, finalFilename, config, gzipWriter); err != nil {
			if removeErr := os.Remove(filename); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("remove uncompressed temporary file: %w", removeErr))
			}
			tb.Fatalf("Failed to generate gzip file: %v", err)
		}
		if err := os.Remove(filename); err != nil {
			if removeErr := os.Remove(finalFilename); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("remove generated gzip file: %w", removeErr))
			}
			tb.Fatalf("Failed to remove uncompressed temporary file: %v", err)
		}
		return finalFilename
	case ZstdCompression:
		finalFilename = filename + ".zst"
		if err := generateCompressedFile(filename, finalFilename, config, zstdWriter); err != nil {
			if removeErr := os.Remove(filename); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("remove uncompressed temporary file: %w", removeErr))
			}
			tb.Fatalf("Failed to generate zstd file: %v", err)
		}
		if err := os.Remove(filename); err != nil {
			if removeErr := os.Remove(finalFilename); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("remove generated zstd file: %w", removeErr))
			}
			tb.Fatalf("Failed to remove uncompressed temporary file: %v", err)
		}
		return finalFilename
	default:
		if err := generateUncompressedFile(filename, config); err != nil {
			if removeErr := os.Remove(filename); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("remove incomplete output: %w", removeErr))
			}
			tb.Fatalf("Failed to generate file: %v", err)
		}
		return filename
	}
}

// generateUncompressedFile creates an uncompressed log file
func generateUncompressedFile(filename string, config TestDataConfig) error {
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}

	writer := bufio.NewWriter(file)
	writeErr := writeLogLines(writer, config)
	flushErr := writer.Flush()
	closeErr := file.Close()
	return errors.Join(
		wrapError("write log data", writeErr),
		wrapError("flush log data", flushErr),
		wrapError("close output file", closeErr),
	)
}

// compressionWriter is a function that creates a compression writer
type compressionWriter func(io.Writer) (io.WriteCloser, error)

// gzipWriter creates a gzip writer
func gzipWriter(w io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriter(w), nil
}

// zstdWriter creates a zstd writer
func zstdWriter(w io.Writer) (io.WriteCloser, error) {
	return zstd.NewWriterLevel(w, zstd.DefaultCompression), nil
}

// generateCompressedFile creates a compressed log file
func generateCompressedFile(tmpFile, finalFile string, config TestDataConfig, createWriter compressionWriter) error {
	// First generate uncompressed
	if err := generateUncompressedFile(tmpFile, config); err != nil {
		return err
	}

	// Read and compress
	input, err := os.Open(tmpFile)
	if err != nil {
		return fmt.Errorf("open uncompressed data: %w", err)
	}

	output, err := os.Create(finalFile)
	if err != nil {
		return errors.Join(
			fmt.Errorf("create compressed output: %w", err),
			wrapError("close uncompressed input", input.Close()),
		)
	}

	compressor, err := createWriter(output)
	if err != nil {
		resultErr := errors.Join(
			fmt.Errorf("create compression writer: %w", err),
			wrapError("close compressed output", output.Close()),
			wrapError("close uncompressed input", input.Close()),
		)
		if removeErr := os.Remove(finalFile); removeErr != nil && !os.IsNotExist(removeErr) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete compressed output: %w", removeErr))
		}
		return resultErr
	}

	_, copyErr := io.Copy(compressor, input)
	resultErr := errors.Join(
		wrapError("compress data", copyErr),
		wrapError("finish compressed data", compressor.Close()),
		wrapError("close compressed output", output.Close()),
		wrapError("close uncompressed input", input.Close()),
	)
	if resultErr != nil {
		if removeErr := os.Remove(finalFile); removeErr != nil && !os.IsNotExist(removeErr) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete compressed output: %w", removeErr))
		}
	}
	return resultErr
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// writeLogLines generates log content based on config
func writeLogLines(w io.Writer, config TestDataConfig) error {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Calculate approximate lines needed
	avgLineSize := 150 // bytes
	totalLines := int(config.Size) / avgLineSize

	// Pre-generate some template lines for variation
	templateLines := generateTemplateLines(config.Format, config.LineVariation, config.Pattern, config.PatternRate, rng)

	bytesWritten := 0
	for i := 0; i < totalLines && bytesWritten < int(config.Size); i++ {
		// Pick a random template line
		line := templateLines[rng.Intn(len(templateLines))]

		// Write with current timestamp
		timestampedLine := strings.Replace(line, "{TIMESTAMP}", generateTimestamp(i), 1)
		timestampedLine = strings.Replace(timestampedLine, "{COUNTER}", fmt.Sprintf("%d", i), 1)

		n, err := fmt.Fprintln(w, timestampedLine)
		if err != nil {
			return err
		}
		bytesWritten += n
	}

	return nil
}

// generateTemplateLines creates a set of template log lines
func generateTemplateLines(format LogFormat, variation int, pattern string, patternRate int, rng *rand.Rand) []string {
	numTemplates := max(10, variation) // At least 10 templates
	templates := make([]string, 0, numTemplates)

	for i := 0; i < numTemplates; i++ {
		includePattern := pattern != "" && rng.Intn(100) < patternRate

		switch format {
		case SimpleLogFormat:
			templates = append(templates, generateSimpleLogLine(i, includePattern, pattern, rng))
		case MapReduceLogFormat:
			templates = append(templates, generateMapReduceLogLine(includePattern, pattern, rng))
		case MixedLogFormat:
			if rng.Intn(2) == 0 {
				templates = append(templates, generateSimpleLogLine(i, includePattern, pattern, rng))
			} else {
				templates = append(templates, generateMapReduceLogLine(includePattern, pattern, rng))
			}
		}
	}

	return templates
}

// generateSimpleLogLine creates a simple log line template
func generateSimpleLogLine(id int, includePattern bool, pattern string, rng *rand.Rand) string {
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	level := levels[rng.Intn(len(levels))]

	message := fmt.Sprintf("Processing request %d", id)
	if includePattern && pattern != "" {
		message = fmt.Sprintf("%s %s", message, pattern)
	}

	// Format: LEVEL|TIMESTAMP|THREAD|FILE:LINE|MESSAGE
	return fmt.Sprintf("%s|{TIMESTAMP}|thread-%d|app.go:%d|%s",
		level, rng.Intn(10)+1, rng.Intn(1000)+1, message)
}

// generateMapReduceLogLine creates a MapReduce format log line template
func generateMapReduceLogLine(includePattern bool, pattern string, rng *rand.Rand) string {
	goroutines := rng.Intn(50) + 10
	connections := rng.Intn(100)
	lifetime := rng.Intn(1000) + 100

	message := "MAPREDUCE:STATS"
	if includePattern && pattern != "" {
		message = fmt.Sprintf("%s|%s", message, pattern)
	}

	// Format matching the integration test data
	return fmt.Sprintf("INFO|{TIMESTAMP}|1|stats.go:56|8|%d|7|0.%02d|471h%dm%ds|%s|currentConnections=%d|lifetimeConnections=%d",
		goroutines, rng.Intn(100), rng.Intn(60), rng.Intn(60), message, connections, lifetime)
}

// generateTimestamp creates a timestamp for log lines
func generateTimestamp(lineNum int) string {
	// Format: MMDD-HHMMSS
	baseTime := time.Date(2024, 10, 2, 7, 10, 0, 0, time.UTC)
	offsetSeconds := lineNum / 10 // Advance time every 10 lines
	t := baseTime.Add(time.Duration(offsetSeconds) * time.Second)
	return t.Format("0102-150405")
}

// CleanupBenchmarkFiles removes all benchmark temporary files
func CleanupBenchmarkFiles(pattern string) error {
	if pattern == "" {
		pattern = "dtail_bench_*.tmp*"
	}

	tempDir := os.TempDir()
	matches, err := filepath.Glob(filepath.Join(tempDir, pattern))
	if err != nil {
		return err
	}

	for _, match := range matches {
		if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}
