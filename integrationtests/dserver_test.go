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

	csvFile := "dserver1.csv"
	expectedCsvFile := "dserver1.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dserver1.csv.query.expected"

	// In case files still exists from previous test run.
	os.Remove(csvFile)
	os.Remove(queryFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "dserver1.cfg",
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", "127.0.0.1",
		"--shutdownAfter", "5",
		"--port", fmt.Sprintf("%d", getUniquePortNumber()),
	)
	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFiles(t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(csvFile)
	os.Remove(queryFile)
}

func TestDServer2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	// Testing a continious query.

	inFile := "dserver2.log"
	csvFile := "dserver2.csv"
	expectedCsvFile := "dserver2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dserver2.csv.query.expected"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
				fd.WriteString(strings.Join(parts, "|"))
				fd.WriteString("\n")
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
		"--shutdownAfter", "7",
		"--port", fmt.Sprintf("%d", getUniquePortNumber()),
	)
	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)
	cancel()

	if err := compareFiles(t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(inFile)
	os.Remove(csvFile)
	os.Remove(queryFile)
}
