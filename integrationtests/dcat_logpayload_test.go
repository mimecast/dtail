package integrationtests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// payloadMarker is the last line of dcat1a.txt. It is distinctive enough that
// its presence in a file proves the retrieved payload was written there.
const payloadMarker = "500 Sat  2 Oct 13:46:46 EEST 2021"

// readDailyLog concatenates every YYYYMMDD.log file the fout logger may have
// written into dir. A missing/empty dir returns "" (the default logger only
// creates the file on the first write, so a payload-free run may leave none).
func readDailyLog(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatalf("globbing log dir %s: %v", dir, err)
	}
	var sb strings.Builder
	for _, m := range matches {
		content, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("reading log file %s: %v", m, err)
		}
		sb.Write(content)
	}
	return sb.String()
}

// TestDCatLogPayload verifies Option B of task dt0: the default client log file
// (the fout logger's daily file) records diagnostics only, and payload teeing
// is opt-in via --log-payload. All sub-tests run serverless so the client log
// output is deterministic (no network timing in diagnostics).
func TestDCatLogPayload(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCatLogPayload")
	defer testLogger.WriteLogFile()

	inFile := "dcat1a.txt"

	// Default: file must be diagnostics-only, no payload; payload still on stdout.
	t.Run("DefaultDiagnosticsOnlyNoPayload", func(t *testing.T) {
		logDir := t.TempDir()
		outFile := "dcatlogpayload_default.tmp"
		ctx := WithTestLogger(context.Background(), testLogger)

		if _, err := runCommand(ctx, t, outFile, "../dcat",
			"--logger", "fout", "--logDir", logDir, "--logLevel", "debug",
			"--noColor", "--cfg", "none", inFile); err != nil {
			t.Fatal(err)
		}

		logContent := readDailyLog(t, logDir)
		if strings.Contains(logContent, payloadMarker) {
			t.Fatalf("payload leaked into the daily client log file by default (found %q)", payloadMarker)
		}
		// Diagnostics must still land in the file: in integration mode every
		// client diagnostic line carries the "integrationtest" hostname token.
		if !strings.Contains(logContent, "integrationtest") {
			t.Fatalf("expected diagnostics in the daily log file, got none:\n%s", logContent)
		}
		// Payload must still reach the terminal/stdout.
		stdout := readTmpFile(t, outFile)
		if !strings.Contains(stdout, payloadMarker) {
			t.Fatal("payload missing from stdout; it must always reach the terminal")
		}
	})

	// Opt-in: --log-payload restores the legacy full tee into the file.
	t.Run("OptInTeesPayloadToFile", func(t *testing.T) {
		logDir := t.TempDir()
		outFile := "dcatlogpayload_optin.tmp"
		ctx := WithTestLogger(context.Background(), testLogger)

		if _, err := runCommand(ctx, t, outFile, "../dcat",
			"--logger", "fout", "--logDir", logDir, "--logLevel", "debug",
			"--noColor", "--log-payload", "--cfg", "none", inFile); err != nil {
			t.Fatal(err)
		}

		logContent := readDailyLog(t, logDir)
		if !strings.Contains(logContent, payloadMarker) {
			t.Fatalf("expected payload teed into the daily log file with --log-payload, not found")
		}
	})

	// Stdout must be byte-identical with and without --log-payload: only the
	// FILE content changes, never what the user sees on the terminal.
	t.Run("StdoutByteIdenticalWithAndWithoutFlag", func(t *testing.T) {
		defaultLogDir := t.TempDir()
		optinLogDir := t.TempDir()
		defaultOut := "dcatlogpayload_stdout_default.tmp"
		optinOut := "dcatlogpayload_stdout_optin.tmp"
		ctx := WithTestLogger(context.Background(), testLogger)

		if _, err := runCommand(ctx, t, defaultOut, "../dcat",
			"--plain", "--logger", "fout", "--logDir", defaultLogDir,
			"--cfg", "none", inFile); err != nil {
			t.Fatal(err)
		}
		if _, err := runCommand(ctx, t, optinOut, "../dcat",
			"--plain", "--logger", "fout", "--logDir", optinLogDir,
			"--log-payload", "--cfg", "none", inFile); err != nil {
			t.Fatal(err)
		}

		if got, want := readTmpFile(t, optinOut), readTmpFile(t, defaultOut); got != want {
			t.Fatalf("stdout differs between default and --log-payload runs:\n got %q\nwant %q", got, want)
		}

		// Sanity: file behavior still toggled underneath the identical stdout.
		if strings.Contains(readDailyLog(t, defaultLogDir), payloadMarker) {
			t.Fatal("default run leaked payload into the log file")
		}
		if !strings.Contains(readDailyLog(t, optinLogDir), payloadMarker) {
			t.Fatal("--log-payload run did not tee payload into the log file")
		}
	})
}

// readTmpFile reads a captured stdout file produced by runCommand.
func readTmpFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(content)
}
