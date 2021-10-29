package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDServer(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	csvFile := "dserver.csv"
	expectedCsvFile := "dserver.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dserver.csv.query.expected"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"../dserver",
		"--cfg", "dserver.cfg",
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", "localhost",
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
