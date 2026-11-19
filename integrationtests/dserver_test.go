package integrationtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func TestDServer1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	// Testing a scheduled query.
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDServer1")
	defer testLogger.WriteLogFile()

	csvFile := "dserver1.csv.tmp"
	expectedCsvFile := "dserver1.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dserver1.csv.query.expected"

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := WithTestLogger(baseCtx, testLogger)

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "dserver1.cfg",
		"--logger", "stdout",
		"--logLevel", "debug",
		"--bindAddress", "127.0.0.1",
		"--shutdownAfter", "10",
		"--port", fmt.Sprintf("%d", getUniquePortNumber()),
	)
	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFilesWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFilesWithContext(ctx, t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}
}

func TestDServer2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	// Testing a continious query.
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDServer2")
	defer testLogger.WriteLogFile()

	inFile := "dserver2.log.tmp"
	csvFile := "dserver2.csv.tmp"
	expectedCsvFile := "dserver2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dserver2.csv.query.expected"

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := WithTestLogger(baseCtx, testLogger)

	fd, err := os.Create(inFile)
	if err != nil {
		t.Error(err)
	}
	defer fd.Close()

	go func() {
		for {
			select {
			case <-time.After(time.Second):
				parts := []string{"INFO", "19801011-424242", "1", "dserver_test.go",
					"1", "1", "1", "1.0", "1m", "MAPREDUCE:INTEGRATIONTEST",
					"foo=1", "bar=42"}
				_, _ = fd.WriteString(strings.Join(parts, "|"))
				_, _ = fd.WriteString("\n")
			case <-ctx.Done():
				return
			}
		}
	}()

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "dserver2.cfg",
		"--logger", "stdout",
		"--logLevel", "debug",
		"--bindAddress", "127.0.0.1",
		"--shutdownAfter", "10",
		"--port", fmt.Sprintf("%d", getUniquePortNumber()),
	)
	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)
	cancel()

	if err := compareFilesWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFilesWithContext(ctx, t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}
}