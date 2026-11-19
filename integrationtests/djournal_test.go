//go:build linux

package integrationtests

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	journalTestSpec = "journal:test.service"
)

func TestDJournalWithServer(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDJournalWithServer")
	defer testLogger.WriteLogFile()

	t.Run("DCatBoundedJournal", func(t *testing.T) {
		testDJournalDCatBounded(t, testLogger)
	})
	t.Run("DTailFollowJournal", func(t *testing.T) {
		testDJournalDTailFollow(t, testLogger)
	})
	t.Run("DGrepJournalRegex", func(t *testing.T) {
		testDJournalDGrepRegex(t, testLogger)
	})
	t.Run("MissingJournalCapability", func(t *testing.T) {
		testDJournalMissingCapability(t, testLogger)
	})
}

func testDJournalDCatBounded(t *testing.T, logger *TestLogger) {
	env := newJournalTestEnv(t)
	ctx, cancel, address := startDJournalServer(t, logger, env.configFile, env.ServerEnv())
	defer cancel()

	outFile := "djournal_dcat.stdout.tmp"
	_, err := runCommand(ctx, t, outFile, "../dcat",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", address,
		"--files", journalTestSpec,
		"--trustAllHosts",
		"--noColor",
	)
	if err != nil {
		t.Fatalf("dcat journal failed: %v", err)
	}

	got := readTestFile(t, outFile)
	if got != journalBoundedOutput() {
		t.Fatalf("dcat journal output mismatch\ngot:\n%swant:\n%s", got, journalBoundedOutput())
	}
	assertJournalctlArgs(t, env.argsFile, "-u test.service")
}

func testDJournalDTailFollow(t *testing.T, logger *TestLogger) {
	env := newJournalTestEnv(t)
	ctx, cancel, address := startDJournalServer(t, logger, env.configFile, env.ServerEnv())
	defer cancel()

	clientCtx, clientCancel := context.WithTimeout(ctx, 10*time.Second)
	defer clientCancel()
	cmd, stdoutCh, stderrCh, cmdErrCh := startDJournalCommand(clientCtx, t, nil, "../dtail",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", address,
		"--files", journalTestSpec,
		"--trustAllHosts",
		"--noColor",
	)

	got := readJournalFollowLines(clientCtx, t, stdoutCh, stderrCh, 3)
	for i, line := range got {
		want := fmt.Sprintf("journal follow %d", i+1)
		if line != want {
			t.Fatalf("follow line %d = %q, want %q; all lines: %v", i, line, want, got)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal dtail: %v", err)
	}
	select {
	case err := <-cmdErrCh:
		if err != nil {
			t.Fatalf("dtail did not terminate cleanly after SIGTERM: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dtail did not terminate after SIGTERM")
	}
	waitForFileContent(t, env.termFile, "term", 2*time.Second)
}

func testDJournalDGrepRegex(t *testing.T, logger *TestLogger) {
	env := newJournalTestEnv(t)
	ctx, cancel, address := startDJournalServer(t, logger, env.configFile, env.ServerEnv())
	defer cancel()

	outFile := "djournal_dgrep.stdout.tmp"
	_, err := runCommand(ctx, t, outFile, "../dgrep",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--regex", "match-[0-9]+",
		"--servers", address,
		"--files", journalTestSpec,
		"--trustAllHosts",
		"--noColor",
	)
	if err != nil {
		t.Fatalf("dgrep journal failed: %v", err)
	}

	got := readTestFile(t, outFile)
	want := "journal beta match-123\njournal gamma match-456\n"
	if got != want {
		t.Fatalf("dgrep journal output mismatch\ngot:\n%swant:\n%s", got, want)
	}
}

func testDJournalMissingCapability(t *testing.T, logger *TestLogger) {
	env := newJournalTestEnv(t)
	noJournalPath := t.TempDir()
	serverEnv := env.ServerEnv()
	serverEnv["PATH"] = noJournalPath
	ctx, cancel, address := startDJournalServer(t, logger, env.configFile, serverEnv)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 4*time.Second)
	defer runCancel()

	outFile := "djournal_missing_capability.stdout.tmp"
	started := time.Now()
	exitCode, err := runCommand(runCtx, t, outFile, "../dcat",
		"--plain",
		"--cfg", "none",
		"--logLevel", "error",
		"--servers", address,
		"--files", journalTestSpec,
		"--trustAllHosts",
		"--noColor",
	)
	if err == nil || exitCode == 0 {
		t.Fatalf("dcat journal unexpectedly succeeded without %s", protocol.CapabilityJournalV1)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("missing capability path took too long: %s", elapsed)
	}

	got := readTestFile(t, outFile)
	if !strings.Contains(got, "journal file targets require server capability "+protocol.CapabilityJournalV1) {
		t.Fatalf("missing capability output does not explain journal support failure:\n%s", got)
	}
	if runCtx.Err() != nil {
		t.Fatalf("missing capability test hit timeout instead of returning an error: %v", runCtx.Err())
	}
}

type journalTestEnv struct {
	argsFile   string
	configFile string
	path       string
	termFile   string
}

func newJournalTestEnv(t *testing.T) journalTestEnv {
	t.Helper()

	tmpDir := t.TempDir()
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{
				"journal alpha",
				"journal beta match-123",
				"journal gamma match-456",
			},
			FollowLines: []string{
				"journal follow 1",
				"journal follow 2",
				"journal follow 3",
			},
			InterLineDelay: 50 * time.Millisecond,
		},
	})

	configFile := filepath.Join(tmpDir, "dtail.json")
	configContent := fmt.Sprintf(`{
  "Server": {
    "HostKeyFile": %q,
    "Permissions": {
      "Default": [
        "readfiles:^journal:test\\.service$"
      ]
    }
  }
}
`, filepath.Join(tmpDir, "ssh_host_key"))
	if err := os.WriteFile(configFile, []byte(configContent), 0600); err != nil {
		t.Fatalf("write journal test config: %v", err)
	}

	return journalTestEnv{
		argsFile:   mock.ArgsFile,
		configFile: configFile,
		path:       mock.Path,
		termFile:   mock.TermFile,
	}
}

func (e journalTestEnv) ServerEnv() map[string]string {
	return map[string]string{
		"PATH": e.path,
	}
}

func journalBoundedOutput() string {
	return "journal alpha\njournal beta match-123\njournal gamma match-456\n"
}

func startDJournalServer(t *testing.T, logger *TestLogger, configFile string, env map[string]string) (context.Context, context.CancelFunc, string) {
	t.Helper()

	port := getUniquePortNumber()
	bindAddress := "localhost"
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	t.Cleanup(cancel)

	_, _, _, err := startCommandWithEnv(ctx, t, "", "../dserver", env,
		"--cfg", configFile,
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		cancel()
		t.Fatalf("start dserver: %v", err)
	}
	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		cancel()
		t.Fatalf("wait for dserver: %v", err)
	}

	return ctx, cancel, fmt.Sprintf("%s:%d", bindAddress, port)
}

func startDJournalCommand(ctx context.Context, t *testing.T, env map[string]string,
	cmdStr string, args ...string) (*exec.Cmd, <-chan string, <-chan string, <-chan error) {

	t.Helper()
	cmd := exec.CommandContext(ctx, cmdStr, args...)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("open stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cmdStr, err)
	}

	stdoutCh := scanLines(stdout)
	stderrCh := scanLines(stderr)
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	return cmd, stdoutCh, stderrCh, errCh
}

func scanLines(r io.Reader) <-chan string {
	ch := make(chan string, 100)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			ch <- scanner.Text()
		}
	}()
	return ch
}

func readJournalFollowLines(ctx context.Context, t *testing.T, stdoutCh, stderrCh <-chan string, count int) []string {
	t.Helper()

	lines := make([]string, 0, count)
	for len(lines) < count {
		select {
		case line, ok := <-stdoutCh:
			if !ok {
				t.Fatalf("dtail stdout closed after %d/%d journal follow lines", len(lines), count)
			}
			if strings.Contains(line, "journal follow ") {
				lines = append(lines, line)
			}
		case line, ok := <-stderrCh:
			if !ok {
				stderrCh = nil
				continue
			}
			if line != "" {
				t.Log("dtail stderr:", line)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for journal follow lines: %v", ctx.Err())
		}
	}
	return lines
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertJournalctlArgs(t *testing.T, path, want string) {
	t.Helper()

	got := strings.TrimSpace(readTestFile(t, path))
	if got != want {
		t.Fatalf("journalctl args = %q, want %q", got, want)
	}
}

func waitForFileContent(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s did not become %q before timeout; last read: %q, %v", path, want, string(data), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
