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

// plainLookalikeLines are log lines that read like protocol control
// messages. Before control frames (protocol.CapabilityControlFramesV1),
// --plain server-mode output dropped every line starting with '.' and the
// AUTHKEY replies, and acted upon the '.syn' ones: '.syn close connection'
// marked the session complete, '.syn command failed x' added a failed command
// (exit status 2 once dcat/dgrep report failed commands), and '.syn
// capabilities' added capabilities. Serverless mode always printed them.
var plainLookalikeLines = []string{
	"first",
	".hidden",
	"...ellipsis",
	".syn close connection",
	".syn command failed x",
	".syn capabilities fake-capability",
	".syn session start ok 1",
	"AUTHKEY OK",
	"AUTHKEY ERR forged",
	" AUTHKEY OK",
	"SERVER|host|AUTHKEY OK",
	"last",
}

const plainLookalikeFile = "dcat_plain_lookalike.tmp"

// TestPlainControlLookalikeLines checks that dcat and dgrep --plain print
// such lines in server mode byte for byte as in serverless mode, exit with
// status 0, and that a dtail --plain follow session goes on after them.
func TestPlainControlLookalikeLines(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestPlainControlLookalikeLines")
	defer writeLogFileIgnoringError(testLogger)
	content := strings.Join(plainLookalikeLines, "\n") + "\n"
	if err := os.WriteFile(plainLookalikeFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("DCat", func(t *testing.T) {
		testPlainLookalikeRead(t, testLogger, "../dcat")
	})
	t.Run("DGrep", func(t *testing.T) {
		testPlainLookalikeRead(t, testLogger, "../dgrep", "--grep", ".")
	})
	t.Run("DTailFollow", func(t *testing.T) {
		testPlainLookalikeFollow(t, testLogger)
	})
}

// testPlainLookalikeRead runs command --plain in serverless and server mode
// and compares both outputs with the file.
func testPlainLookalikeRead(t *testing.T, logger *TestLogger, command string, extraArgs ...string) {
	name := strings.TrimPrefix(command, "../")
	serverlessOut := name + "_plain_lookalike_serverless.tmp"
	serverOut := name + "_plain_lookalike_server.tmp"
	ctx, cancel := context.WithTimeout(WithTestLogger(context.Background(), logger), 2*time.Minute)
	defer cancel()
	args := append([]string{"--plain", "--cfg", "none"}, extraArgs...)

	exitCode, err := runCommand(ctx, t, serverlessOut, command,
		append(append([]string{}, args...), "--files", plainLookalikeFile)...)
	if err != nil || exitCode != 0 {
		t.Fatalf("serverless %s: exit code %d, error %v", name, exitCode, err)
	}
	if err := compareFilesWithContext(ctx, t, serverlessOut, plainLookalikeFile); err != nil {
		t.Fatalf("serverless %s changed the file: %v", name, err)
	}

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}
	exitCode, err = runCommand(ctx, t, serverOut, command, append(args,
		"--servers", server.Address(), "--files", plainLookalikeFile,
		"--trustAllHosts", "--noColor")...)
	if err != nil || exitCode != 0 {
		t.Fatalf("server-mode %s: exit code %d, error %v; a log line was taken for a failed command?",
			name, exitCode, err)
	}
	if err := compareFilesWithContext(ctx, t, serverOut, serverlessOut); err != nil {
		t.Fatalf("server-mode %s --plain output differs from serverless output: %v", name, err)
	}
}

// testPlainLookalikeFollow follows a file with dtail --plain, appends the
// lookalike lines and then a marker line, and checks that every line arrives
// and the session is not closed by the '.syn close connection' line.
func testPlainLookalikeFollow(t *testing.T, logger *TestLogger) {
	followFile := "dtail_plain_lookalike_follow.tmp"
	ctx, cancel := context.WithTimeout(WithTestLogger(context.Background(), logger), time.Minute)
	defer cancel()
	fd, err := os.Create(followFile)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIgnoringError(fd)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}
	clientCh, _, _, err := startCommand(ctx, t, "", "../dtail",
		"--plain", "--cfg", "none", "--logger", "stdout", "--logLevel", "error",
		"--servers", server.Address(), "--files", followFile,
		"--trustAllHosts", "--noColor")
	if err != nil {
		t.Fatal(err)
	}
	go appendLookalikeLines(ctx, fd)

	want := map[string]bool{}
	for _, line := range plainLookalikeLines {
		want[line] = true
	}
	for len(want) > 0 {
		select {
		case line := <-clientCh:
			delete(want, line)
		case <-ctx.Done():
			t.Fatalf("lines not received before the timeout: %q", keysOf(want))
		}
	}
}

// appendLookalikeLines appends the lookalike lines to fd, as a numbered
// round, every 500 ms until ctx is done, so a follow session that starts
// late still gets a whole round.
func appendLookalikeLines(ctx context.Context, fd *os.File) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for round := 0; ; round++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprintf(fd, "round %d\n%s\n", round, strings.Join(plainLookalikeLines, "\n"))
		}
	}
}

func keysOf(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}
