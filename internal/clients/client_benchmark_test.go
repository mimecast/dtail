package clients

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

func setupBenchmarkData(b *testing.B, lines int) string {
	b.Helper()

	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "benchmark_data.log")

	f, err := os.Create(testFile)
	if err != nil {
		b.Fatalf("Failed to create test file: %v", err)
	}

	// Create test data
	for i := 0; i < lines; i++ {
		line := fmt.Sprintf("INFO|1002-071143|1|test.go:%d|8|%d|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=%d|lifetimeConnections=%d|pattern=test-%d|data=%s\n",
			i%100, i%50, i%10, i, i%5, "some-test-data-that-makes-the-line-longer")
		if _, err := f.WriteString(line); err != nil {
			_ = f.Close()
			b.Fatalf("write test data: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		b.Fatalf("close test data: %v", err)
	}

	return testFile
}

func BenchmarkDGrep(b *testing.B) {
	benchmarkDGrepWithSize(b, 100000) // 100k lines
}

// Benchmark with different file sizes
func BenchmarkDGrepSmallFile(b *testing.B) {
	benchmarkDGrepWithSize(b, 1000) // 1k lines
}

func BenchmarkDGrepMediumFile(b *testing.B) {
	benchmarkDGrepWithSize(b, 50000) // 50k lines
}

func BenchmarkDGrepLargeFile(b *testing.B) {
	benchmarkDGrepWithSize(b, 500000) // 500k lines
}

func benchmarkDGrepWithSize(b *testing.B, lines int) {
	// Setup config. The direct-output read path is the only runtime path.
	runtimeCfg := config.RuntimeConfig{
		Server: &config.ServerConfig{
			MaxConcurrentCats:  10,
			MaxConcurrentTails: 50,
			MaxLineLength:      1024 * 1024,
		},
		Common: &config.CommonConfig{
			HostnameOverride: "benchmark-host",
			Logger:           "none",
			LogLevel:         "error",
		},
		Client: &config.ClientConfig{TermColorsEnable: false},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loggers := NewLoggerDependencies(clientlog.NopLogger{}, logging.NopLogger{}, logging.NopLogger{})

	// Create test data
	testFile := setupBenchmarkData(b, lines)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Create grep client
		args := config.Args{
			ServersStr: "serverless",
			QueryStr:   "",
			What:       testFile,
			RegexStr:   "pattern=test-1",
			Serverless: true,
			Plain:      true,
		}

		client, err := NewGrepClient(args, runtimeCfg, loggers)
		if err != nil {
			b.Fatalf("Failed to create grep client: %v", err)
		}

		// Capture output
		oldStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			b.Fatalf("create stdout pipe: %v", err)
		}
		os.Stdout = w

		// Run grep
		statusCh := make(chan int, 1)
		go func() {
			status := client.Start(ctx, nil) // nil for statsCh
			statusCh <- status
		}()

		// Wait for completion or timeout
		select {
		case status := <-statusCh:
			if status != 0 {
				b.Errorf("Grep failed with status: %d", status)
			}
		case <-time.After(30 * time.Second):
			b.Error("Grep timed out")
		}

		// Restore stdout
		os.Stdout = oldStdout
		if err := w.Close(); err != nil {
			_ = r.Close()
			b.Fatalf("close stdout writer: %v", err)
		}

		// Read captured output
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r); err != nil {
			_ = r.Close()
			b.Fatalf("read captured output: %v", err)
		}
		if err := r.Close(); err != nil {
			b.Fatalf("close stdout reader: %v", err)
		}
	}

	// Report custom metrics
	b.ReportMetric(float64(lines), "lines/op")
	b.ReportMetric(float64(lines)/b.Elapsed().Seconds(), "lines/sec")
}
