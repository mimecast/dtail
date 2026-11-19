package common

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// DataFormat represents the format of generated data
type DataFormat string

// Supported data generator output formats.
const (
	// FormatLog generates generic log lines.
	FormatLog       DataFormat = "log"
	FormatCSV       DataFormat = "csv"
	FormatDTail     DataFormat = "dtail"
	FormatMapReduce DataFormat = "mapreduce"
)

// DataGenerator generates test data for profiling and benchmarking
type DataGenerator struct {
	rand *rand.Rand
}

// NewDataGenerator creates a new data generator
func NewDataGenerator() *DataGenerator {
	return &DataGenerator{
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// GenerateFile generates a test data file of the specified size and format
func (g *DataGenerator) GenerateFile(filename string, sizeStr string, format DataFormat) error {
	size, err := ParseSize(sizeStr)
	if err != nil {
		return fmt.Errorf("invalid size: %w", err)
	}

	// Create directory if needed
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Check if file already exists
	if _, err := os.Stat(filename); err == nil {
		return nil // File exists, skip generation
	}

	switch format {
	case FormatLog:
		return g.generateLogFile(filename, size)
	case FormatCSV:
		return g.generateCSVFile(filename, size)
	case FormatDTail, FormatMapReduce:
		return g.generateDTailFormatFile(filename, size)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}
}

// GenerateLogFileWithLines generates a log file with specific number of lines
func (g *DataGenerator) GenerateLogFileWithLines(filename string, lines int, format DataFormat) error {
	// Create directory if needed
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Check if file already exists
	if _, err := os.Stat(filename); err == nil {
		return nil // File exists, skip generation
	}

	switch format {
	case FormatDTail, FormatMapReduce:
		return g.generateDTailFormatFileWithLines(filename, lines)
	default:
		return fmt.Errorf("line-based generation only supported for dtail/mapreduce format")
	}
}

func (g *DataGenerator) generateLogFile(filename string, targetSize int64) (retErr error) {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	defer func() { retErr = errors.Join(retErr, finishGeneratedFile(file, writer, filename)) }()

	var currentSize int64
	lineNum := 0
	levels := []string{"INFO", "DEBUG", "WARN", "ERROR"}
	users := []string{"user1", "user2", "user3", "user4", "user5", "admin", "guest", "service", "monitor", "test"}
	actions := []string{"login", "logout", "query", "update", "delete", "create", "read", "write", "sync", "backup"}

	for currentSize < targetSize {
		lineNum++
		timestamp := time.Now().Add(time.Duration(-lineNum) * time.Second).Format("2006-01-02 15:04:05")
		level := levels[g.rand.Intn(len(levels))]
		user := users[g.rand.Intn(len(users))]
		action := actions[g.rand.Intn(len(actions))]
		duration := g.rand.Intn(5000) + 100
		status := "success"
		if g.rand.Float32() < 0.1 {
			status = "failure"
		}

		line := fmt.Sprintf("[%s] %s - User %s performed %s action (duration: %dms, status: %s)\n",
			timestamp, level, user, action, duration, status)

		n, err := writer.WriteString(line)
		if err != nil {
			return err
		}
		currentSize += int64(n)
	}

	return nil
}

func (g *DataGenerator) generateCSVFile(filename string, targetSize int64) (retErr error) {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	defer func() { retErr = errors.Join(retErr, finishGeneratedFile(file, writer, filename)) }()

	// Write header
	header := "timestamp,user,action,duration,status\n"
	n, err := writer.WriteString(header)
	if err != nil {
		return err
	}
	currentSize := int64(n)

	lineNum := 0
	users := []string{"user1", "user2", "user3", "user4", "user5", "admin", "guest", "service", "monitor", "test"}
	actions := []string{"login", "logout", "query", "update", "delete", "create", "read", "write", "sync", "backup"}

	for currentSize < targetSize {
		lineNum++
		timestamp := time.Now().Add(time.Duration(-lineNum) * time.Second).Format("2006-01-02 15:04:05")
		user := users[g.rand.Intn(len(users))]
		action := actions[g.rand.Intn(len(actions))]
		duration := g.rand.Intn(5000) + 100
		status := "success"
		if g.rand.Float32() < 0.1 {
			status = "failure"
		}

		line := fmt.Sprintf("%s,%s,%s,%d,%s\n", timestamp, user, action, duration, status)

		n, err := writer.WriteString(line)
		if err != nil {
			return err
		}
		currentSize += int64(n)
	}

	return nil
}

func (g *DataGenerator) generateDTailFormatFile(filename string, targetSize int64) (retErr error) {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	defer func() { retErr = errors.Join(retErr, finishGeneratedFile(file, writer, filename)) }()

	var currentSize int64
	lineNum := 0
	hostnames := []string{"server01", "server02", "server03", "server04", "server05",
		"server06", "server07", "server08", "server09", "server10"}

	for currentSize < targetSize {
		lineNum++
		hostname := hostnames[lineNum%len(hostnames)]
		timestamp := fmt.Sprintf("%02d%02d-%02d%02d%02d",
			10+(lineNum/86400)%12, (lineNum/3600)%30+1,
			(lineNum/3600)%24, (lineNum/60)%60, lineNum%60)
		goroutines := 10 + (lineNum % 50)
		cgocalls := lineNum % 100
		cpus := 1 + (lineNum % 8)
		loadavg := float64(lineNum%100) / 100.0
		uptime := fmt.Sprintf("%dh%dm%ds", lineNum/3600, (lineNum/60)%60, lineNum%60)
		currentConnections := lineNum % 20
		lifetimeConnections := 1000 + lineNum

		line := fmt.Sprintf("INFO|%s|1|stats.go:56|%d|%d|%d|%.2f|%s|MAPREDUCE:STATS|hostname=%s|currentConnections=%d|lifetimeConnections=%d\n",
			timestamp, cpus, goroutines, cgocalls, loadavg, uptime, hostname, currentConnections, lifetimeConnections)

		n, err := writer.WriteString(line)
		if err != nil {
			return err
		}
		currentSize += int64(n)
	}

	return nil
}

func (g *DataGenerator) generateDTailFormatFileWithLines(filename string, lines int) (retErr error) {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	defer func() { retErr = errors.Join(retErr, finishGeneratedFile(file, writer, filename)) }()

	hostnames := []string{"server01", "server02", "server03", "server04", "server05",
		"server06", "server07", "server08", "server09", "server10"}

	for i := 1; i <= lines; i++ {
		hostname := hostnames[i%len(hostnames)]
		timestamp := fmt.Sprintf("%02d%02d-%02d%02d%02d",
			10+(i/86400)%12, (i/3600)%30+1,
			(i/3600)%24, (i/60)%60, i%60)
		goroutines := 10 + (i % 50)
		cgocalls := i % 100
		cpus := 1 + (i % 8)
		loadavg := float64(i%100) / 100.0
		uptime := fmt.Sprintf("%dh%dm%ds", i/3600, (i/60)%60, i%60)
		currentConnections := i % 20
		lifetimeConnections := 1000 + i

		line := fmt.Sprintf("INFO|%s|1|stats.go:56|%d|%d|%d|%.2f|%s|MAPREDUCE:STATS|hostname=%s|currentConnections=%d|lifetimeConnections=%d\n",
			timestamp, cpus, goroutines, cgocalls, loadavg, uptime, hostname, currentConnections, lifetimeConnections)

		if _, err := writer.WriteString(line); err != nil {
			return err
		}
	}

	return nil
}

func finishGeneratedFile(file *os.File, writer *bufio.Writer, filename string) error {
	var errs []error
	if err := writer.Flush(); err != nil {
		errs = append(errs, fmt.Errorf("flush generated file %q: %w", filename, err))
	}
	if err := file.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close generated file %q: %w", filename, err))
	}
	return errors.Join(errs...)
}

// GenerateLogFile generates a log file with specified number of lines
// This is a convenience function for PGO module
func GenerateLogFile(filename string, lines int) error {
	g := NewDataGenerator()
	// Estimate size based on average line length (about 100 bytes per line)
	estimatedSize := int64(lines * 100)
	return g.generateLogFile(filename, estimatedSize)
}

// GenerateCSVFile generates a CSV file with specified number of lines
// This is a convenience function for PGO module
func GenerateCSVFile(filename string, lines int) error {
	g := NewDataGenerator()
	// Estimate size based on average line length (about 50 bytes per line)
	estimatedSize := int64(lines * 50)
	return g.generateCSVFile(filename, estimatedSize)
}
