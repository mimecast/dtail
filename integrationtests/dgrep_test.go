package integrationtests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func TestDGrep1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrep1")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDGrep1Serverless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDGrep1WithServer(t, testLogger)
	})
}

func testDGrep1Serverless(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrep.stdout.tmp"
	expectedOutFile := "dgrep1.txt.expected"
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dgrep",
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947",
		inFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}

func testDGrep1WithServer(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrep.stdout.tmp"
	expectedOutFile := "dgrep1.txt.expected"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		return compareFilesWithContext(ctx, t, outFile, expectedOutFile)
	},
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts",
		"--noColor",
		"--files", inFile)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

func TestDGrep1Colors(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrep1Colors")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDGrep1ColorsServerless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDGrep1ColorsWithServer(t, testLogger)
	})
}

func testDGrep1ColorsServerless(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrep1colors.stdout.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	// Run without --plain to get colored output
	_, err := runCommand(ctx, t, outFile,
		"../dgrep",
		"--cfg", "none",
		"--grep", "1002-071947",
		inFile)

	if err != nil {
		t.Error(err)
		return
	}

	// Verify it ran successfully and produced output
	info, err := os.Stat(outFile)
	if err != nil {
		t.Error("Output file not created:", err)
		return
	}
	if info.Size() == 0 {
		t.Error("Output file is empty")
		return
	}

	// Verify output contains ANSI color codes
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Error("Failed to read output file:", err)
		return
	}
	if !strings.Contains(string(content), "\033[") {
		t.Error("Output does not contain ANSI color codes")
		return
	}

	// Log verification
	logger.LogFileComparison(outFile, "ANSI color codes", "contains check")
}

func testDGrep1ColorsWithServer(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrep1colors.stdout.tmp"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		info, statErr := os.Stat(outFile)
		if statErr != nil {
			return fmt.Errorf("output file not created: %w", statErr)
		}
		if info.Size() == 0 {
			return fmt.Errorf("output file is empty")
		}
		content, readErr := os.ReadFile(outFile)
		if readErr != nil {
			return fmt.Errorf("failed to read output file: %w", readErr)
		}
		if !strings.Contains(string(content), "REMOTE") && !strings.Contains(string(content), "SERVER") {
			preview := string(content)
			if len(preview) > 500 {
				preview = preview[:500]
			}
			return fmt.Errorf("server mode output does not contain server metadata. First 500 chars:\n%s", preview)
		}
		return nil
	},
		"--cfg", "none",
		"--grep", "1002-071947",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts",
		"--files", inFile)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
	logger.LogFileComparison(outFile, "server metadata (REMOTE/SERVER)", "contains check")
}

func TestDGrep2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrep2")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDGrep2Serverless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDGrep2WithServer(t, testLogger)
	})
}

func testDGrep2Serverless(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrep.stdout.tmp"
	expectedOutFile := "dgrep2.txt.expected"
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dgrep",
		"--plain",
		"--cfg", "none",
		"--grep", "1002-07194[789]",
		inFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}

func testDGrep2WithServer(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrep.stdout.tmp"
	expectedOutFile := "dgrep2.txt.expected"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		return compareFilesWithContext(ctx, t, outFile, expectedOutFile)
	},
		"--plain",
		"--cfg", "none",
		"--grep", "1002-07194[789]",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts",
		"--noColor",
		"--files", inFile)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

func TestDGrepContext1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepContext1")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDGrepContext1Serverless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDGrepContext1WithServer(t, testLogger)
	})
}

func testDGrepContext1Serverless(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrepcontext.stdout.tmp"
	expectedOutFile := "dgrepcontext1.txt.expected"
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dgrep",
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947",
		"--before", "2",
		"--after", "3",
		inFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}

func testDGrepContext1WithServer(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrepcontext.stdout.tmp"
	expectedOutFile := "dgrepcontext1.txt.expected"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		return compareFilesWithContext(ctx, t, outFile, expectedOutFile)
	},
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947",
		"--before", "2",
		"--after", "3",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts",
		"--noColor",
		"--files", inFile)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

func TestDGrepContext2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepContext2")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDGrepContext2Serverless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDGrepContext2WithServer(t, testLogger)
	})
}

func testDGrepContext2Serverless(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrepcontext.stdout.tmp"
	expectedOutFile := "dgrepcontext2.txt.expected"
	ctx := WithTestLogger(context.Background(), logger)

	_, err := runCommand(ctx, t, outFile,
		"../dgrep",
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947",
		"--before", "2",
		"--after", "3",
		"--invert",
		inFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}

func testDGrepContext2WithServer(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrepcontext.stdout.tmp"
	expectedOutFile := "dgrepcontext2.txt.expected"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	err = runCommandUntilValid(ctx, t, 5, 200*time.Millisecond, outFile, "../dgrep", func() error {
		return compareFilesWithContext(ctx, t, outFile, expectedOutFile)
	},
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947",
		"--before", "2",
		"--after", "3",
		"--invert",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts",
		"--noColor",
		"--files", inFile)
	cancel()
	if err != nil {
		t.Error(err)
		return
	}
}

func TestDGrepPipeToStdin(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepPipeToStdin")
	defer testLogger.WriteLogFile()

	// Only test in serverless mode (stdin piping doesn't work with server mode)
	t.Run("Serverless", func(t *testing.T) {
		testDGrepStdinServerless(t, testLogger)
	})
}

func testDGrepStdinServerless(t *testing.T, logger *TestLogger) {
	inFile := "mapr_testdata.log"
	outFile := "dgrepstdin.stdout.tmp"
	expectedOutFile := "dgrep1.txt.expected"
	ctx := WithTestLogger(context.Background(), logger)

	// Use startCommand with stdin piping
	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		inFile, // This will be piped to stdin
		"../dgrep",
		"--plain",
		"--cfg", "none",
		"--grep", "1002-071947")

	if err != nil {
		t.Error(err)
		return
	}

	// Collect output
	fd, err := os.Create(outFile)
	if err != nil {
		t.Error(err)
		return
	}
	defer fd.Close()

	// Read from stdout channel
	go func() {
		for line := range stdoutCh {
			fmt.Fprintln(fd, line)
		}
	}()

	// Drain stderr channel
	go func() {
		for line := range stderrCh {
			t.Log("stderr:", line)
		}
	}()

	// Wait for command to complete
	select {
	case <-ctx.Done():
		t.Error("Context cancelled")
		return
	case cmdErr := <-cmdErrCh:
		if cmdErr != nil {
			t.Error("Command failed:", cmdErr)
			return
		}
	case <-time.After(5 * time.Second):
		t.Error("Command timed out")
		return
	}

	// Give time for output to be written
	time.Sleep(100 * time.Millisecond)

	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}

// TestDGrepMaxCountNoEOFError is a regression test for the (former "turbo")
// direct read path leaking the io.EOF early-stop sentinel that
// processWithContext returns once a -max (MaxCount) limit is reached. The leak
// surfaced as a spurious "SERVER|...|ERROR|...|EOF" line.
//
// The direct read path is now the one and only runtime path, so this runs a
// single server. The test asserts the server logged "Using turbo mode for
// reading" (proving the optimized path actually ran) and that the -max output
// carries no ERROR/EOF line and matches the exact expected payload.
func TestDGrepMaxCountNoEOFError(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDGrepMaxCountNoEOFError")
	defer testLogger.WriteLogFile()

	// Many lines match "INFO"; -max 3 forces an early stop well before EOF, which
	// is what makes processWithContext return the io.EOF sentinel.
	dataFile := filepath.Join(t.TempDir(), "maxcount_input.log")
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "INFO line %d\n", i)
	}
	if err := os.WriteFile(dataFile, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}
	configFile := writeTurboFileServerConfig(t, dataFile)

	// run starts a dserver and greps the data file with -max 3.
	run := func(name string, serverEnv map[string]string) (string, *safeLineLog) {
		serverEnv["DTAIL_HOSTNAME_OVERRIDE"] = "integrationtest"
		server := startDJournalExtendedServer(t, testLogger, configFile, serverEnv, "debug", true)
		outFile := fmt.Sprintf("dgrep_maxcount_%s.stdout.tmp", name)
		if _, err := runCommand(server.ctx, t, outFile, "../dgrep",
			"--plain",
			"--cfg", "none",
			"--logLevel", "error",
			"--grep", "INFO",
			"--max", "3",
			"--servers", server.address,
			"--trustAllHosts",
			"--noColor",
			"--files", dataFile,
		); err != nil {
			t.Fatalf("dgrep -max (%s) failed: %v", name, err)
		}
		return readTestFile(t, outFile), server.logs
	}

	turboOut, turboLogs := run("turbo", map[string]string{})

	// In SERVER mode the leaked max-count sentinel does NOT reach the client
	// output (unlike serverless): the server logs it via dlog.Server.Error in
	// executeReadLoop as an "ERROR|...|<file>|<basename>|EOF" line on its own
	// stdout. So the regression is asserted against the server logs, not the
	// client output. assertNoReadEOFError distinguishes that leak (an ERROR line
	// ending in "|EOF") from the benign "ssh: parse error in message type 0"
	// ERROR line the handshake always emits.
	assertNoReadEOFError := func(t *testing.T, label string, logs *safeLineLog) {
		t.Helper()
		for _, ln := range strings.Split(logs.String(), "\n") {
			if strings.Contains(ln, "ERROR") && strings.HasSuffix(ln, "|EOF") {
				t.Fatalf("%s server leaked the max-count io.EOF sentinel as a spurious "+
					"ERROR log line: %q\nfull logs:\n%s", label, ln, logs.String())
			}
		}
	}

	// Prove the run genuinely exercised the optimized path.
	turboLogs.waitContains(t, "Using turbo mode for reading", 5*time.Second)
	// "File processing complete" is logged strictly AFTER the read returns (and
	// thus after any ERROR|...|EOF line from executeReadLoop), so once it appears
	// the absence check below cannot pass merely because the log has not flushed.
	turboLogs.waitContains(t, "File processing complete", 5*time.Second)

	assertNoReadEOFError(t, "turbo", turboLogs)

	if want := "INFO line 0\nINFO line 1\nINFO line 2\n"; turboOut != want {
		t.Fatalf("turbo -max output: got:\n%swant:\n%s", turboOut, want)
	}
}

// writeTurboFileServerConfig creates a dserver config for a turbo-enabled
// integration server that is permitted to read readableFile. It mirrors the auth
// setup used by the journal-extended turbo tests: a CacheDir holding the
// integration public key as <user>.authorized_keys, so the server can run with
// the integration-test force-disable dropped (turbo ON) while the client still
// authenticates with the integration key. Returns the config file path.
func writeTurboFileServerConfig(t *testing.T, readableFile string) string {
	t.Helper()

	tmpDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get test working directory: %v", err)
	}
	// CacheDir is resolved relative to the server's working directory, so the key
	// cache must live under cwd and be referenced by its basename (as the journal
	// harness does).
	cacheDirAbs, err := os.MkdirTemp(cwd, "dgrep-maxcount-key-cache-")
	if err != nil {
		t.Fatalf("create key cache dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cacheDirAbs) })
	if err := os.WriteFile(filepath.Join(cacheDirAbs, currentUsername(t)+".authorized_keys"),
		readIntegrationPublicKey(t), 0o600); err != nil {
		t.Fatalf("write authorized_keys cache: %v", err)
	}

	content, err := json.MarshalIndent(map[string]any{
		"Common": map[string]any{
			"CacheDir": filepath.Base(cacheDirAbs),
		},
		"Server": map[string]any{
			"HostKeyFile": filepath.Join(tmpDir, "ssh_host_key"),
			"Permissions": map[string]any{
				"Default": []string{permissionForPath(readableFile)},
			},
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal server config: %v", err)
	}
	configFile := filepath.Join(tmpDir, "dtail.json")
	if err := os.WriteFile(configFile, append(content, '\n'), 0o600); err != nil {
		t.Fatalf("write server config: %v", err)
	}
	return configFile
}
