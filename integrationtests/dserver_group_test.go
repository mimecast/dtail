package integrationtests

import (
	"context"
	"fmt"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// TestDServerScheduledGroup runs three scheduled jobs on the same file. The
// scheduler runs them together as one group that asks dserver to read the
// file once for all of them; every outfile must be exactly what the job
// writes when it runs on its own (the .expected files were written by a
// dserver that ran the jobs one after another with private reads).
func TestDServerScheduledGroup(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDServerScheduledGroup")
	defer writeLogFileIgnoringError(testLogger)

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := WithTestLogger(baseCtx, testLogger)

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "dserver3.cfg",
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", "127.0.0.1",
		"--shutdownAfter", "10",
		"--port", fmt.Sprintf("%d", getUniquePortNumber()),
	)
	if err != nil {
		t.Error(err)
		return
	}
	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	for _, job := range []string{"dserver3a", "dserver3b", "dserver3c"} {
		csvFile := job + ".csv.tmp"
		if err := compareFilesWithContext(ctx, t, csvFile, job+".csv.expected"); err != nil {
			t.Error(err)
		}
		if err := compareFilesWithContext(ctx, t, csvFile+".query", job+".csv.query.expected"); err != nil {
			t.Error(err)
		}
	}
}
