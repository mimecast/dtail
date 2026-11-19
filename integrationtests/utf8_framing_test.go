package integrationtests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// The UTF-8 encodings of '€' (E2 82 AC), 'Ь' (D0 AC), '¬' (C2 AC) and '丬'
// (E4 B8 AC) end in 0xAC, the byte that delimits protocol messages. Before
// escaped framing (protocol.CapabilityEscapedFramingV1), server mode cut each
// such line at that byte and lost it, while serverless mode printed it whole.
const (
	utf8FramingCatFile         = "dcat_utf8.txt"
	utf8FramingMapInFile       = "dmap_utf8.csv.in"
	utf8FramingMapExpectedFile = "dmap_utf8.csv.expected"
	utf8FramingMapQuery        = "select label,count($line),last(value) group by label logformat csv"
)

// TestUTF8Framing checks that dcat in server mode prints a file with such
// characters byte for byte as serverless dcat and the file itself do, and
// that dmap group keys holding them aggregate correctly in both modes.
func TestUTF8Framing(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestUTF8Framing")
	defer writeLogFileIgnoringError(testLogger)

	t.Run("DCatServerMatchesServerless", func(t *testing.T) {
		testUTF8FramingDCat(t, testLogger)
	})
	t.Run("DCatRemoteFramesMatchServerless", func(t *testing.T) {
		testUTF8FramingRemoteFrames(t, testLogger, "../dcat")
	})
	t.Run("DGrepRemoteFramesMatchServerless", func(t *testing.T) {
		testUTF8FramingRemoteFrames(t, testLogger, "../dgrep", "--grep", "sign|€")
	})
	t.Run("DTailFollow", func(t *testing.T) {
		testUTF8FramingDTailFollow(t, testLogger)
	})
	t.Run("DMapServerless", func(t *testing.T) {
		testUTF8FramingDMap(t, testLogger, false)
	})
	t.Run("DMapServer", func(t *testing.T) {
		testUTF8FramingDMap(t, testLogger, true)
	})
}

func testUTF8FramingDCat(t *testing.T, logger *TestLogger) {
	serverlessOut := "dcat_utf8_serverless.tmp"
	serverOut := "dcat_utf8_server.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	if _, err := runCommand(ctx, t, serverlessOut,
		"../dcat", "--plain", "--cfg", "none", utf8FramingCatFile); err != nil {
		t.Fatal(err)
	}
	if err := compareFilesWithContext(ctx, t, serverlessOut, utf8FramingCatFile); err != nil {
		t.Fatalf("serverless dcat changed the file: %v", err)
	}

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}
	err := runCommandUntilValid(server.ctx, t, 5, 200*time.Millisecond, serverOut, "../dcat",
		func() error { return compareFilesWithContext(ctx, t, serverOut, serverlessOut) },
		"--plain", "--cfg", "none",
		"--servers", server.Address(),
		"--files", utf8FramingCatFile,
		"--trustAllHosts", "--noColor")
	if err != nil {
		t.Fatalf("server-mode dcat output differs from serverless dcat: %v", err)
	}
}

func testUTF8FramingDMap(t *testing.T, logger *TestLogger, withServer bool) {
	mode := "serverless"
	if withServer {
		mode = "server"
	}
	csvFile := fmt.Sprintf("dmap_utf8_%s.csv.tmp", mode)
	outFile := fmt.Sprintf("dmap_utf8_%s.stdout.tmp", mode)
	cleanupFiles(t, csvFile, csvFile+".query", outFile)
	query := fmt.Sprintf("%s outfile %s", utf8FramingMapQuery, csvFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	defer cancel()
	ctx := WithTestLogger(ctxTimeout, logger)
	args := []string{"--cfg", "none", "--query", query, utf8FramingMapInFile}
	if withServer {
		server := NewTestServer(t)
		if err := server.Start("error"); err != nil {
			t.Fatal(err)
		}
		args = []string{"--cfg", "none", "--query", query,
			"--servers", server.Address(), "--files", utf8FramingMapInFile,
			"--trustAllHosts", "--noColor"}
	}
	if _, err := runCommand(ctx, t, outFile, "../dmap", args...); err != nil {
		t.Fatal(err)
	}
	if err := compareFilesContentsWithContext(ctx, t, csvFile, utf8FramingMapExpectedFile); err != nil {
		t.Fatalf("%s dmap result: %v", mode, err)
	}
}

// ansiEscape matches the color escape sequences of serverless output.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// writeWithoutColors writes file without its ANSI color sequences to out, so
// colored serverless output compares with --noColor server-mode output.
func writeWithoutColors(t *testing.T, file, out string) {
	t.Helper()
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, ansiEscape.ReplaceAll(content, nil), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testUTF8FramingRemoteFrames compares the REMOTE frames (non-plain output)
// of a server-mode read with serverless output after client formatting,
// i.e. without the serverless colors, on the file with 0xAC characters.
func testUTF8FramingRemoteFrames(t *testing.T, logger *TestLogger, command string, extraArgs ...string) {
	name := strings.TrimPrefix(command, "../")
	serverlessColored := name + "_utf8_remote_serverless_colored.tmp"
	serverlessOut := name + "_utf8_remote_serverless.tmp"
	serverOut := name + "_utf8_remote_server.tmp"
	ctx := WithTestLogger(context.Background(), logger)

	args := append([]string{"--cfg", "none"}, extraArgs...)
	if _, err := runCommand(ctx, t, serverlessColored, command,
		append(slices.Clone(args), "--files", utf8FramingCatFile)...); err != nil {
		t.Fatal(err)
	}
	writeWithoutColors(t, serverlessColored, serverlessOut)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}
	err := runCommandUntilValid(server.ctx, t, 5, 200*time.Millisecond, serverOut, command,
		func() error { return compareFilesWithContext(ctx, t, serverOut, serverlessOut) },
		append(args, "--servers", server.Address(), "--files", utf8FramingCatFile,
			"--trustAllHosts", "--noColor")...)
	if err != nil {
		t.Fatalf("server-mode %s REMOTE frames differ from serverless output: %v", name, err)
	}
}

// testUTF8FramingDTailFollow follows a file that is appended lines with 0xAC
// characters and checks that every line reaches the client whole.
func testUTF8FramingDTailFollow(t *testing.T, logger *TestLogger) {
	const wantLines = 3
	followFile := "dtail_utf8_follow.tmp"
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
		"--cfg", "none", "--logger", "stdout", "--logLevel", "error",
		"--servers", server.Address(), "--files", followFile,
		"--trustAllHosts", "--noColor")
	if err != nil {
		t.Fatal(err)
	}
	go appendUTF8FollowLines(ctx, fd)

	received := map[string]bool{}
	for len(received) < wantLines {
		select {
		case line := <-clientCh:
			if !strings.Contains(line, "follow ") {
				continue
			}
			_, content, _ := strings.Cut(line, "|"+followFile+"|")
			if !utf8FollowLine.MatchString(content) {
				t.Fatalf("followed line %q is not a whole written line", line)
			}
			received[content] = true
		case <-ctx.Done():
			t.Fatalf("received %d whole lines before the timeout, want %d", len(received), wantLines)
		}
	}
}

// utf8FollowLine matches a whole line written by appendUTF8FollowLines.
var utf8FollowLine = regexp.MustCompile("^follow [0-9]+ 5€ Ь ¬ 丬 end$")

// appendUTF8FollowLines appends a line with 0xAC characters to fd every
// 200 ms until ctx is done.
func appendUTF8FollowLines(ctx context.Context, fd *os.File) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprintf(fd, "follow %d 5€ Ь ¬ 丬 end\n", i)
		}
	}
}

// TestUTF8FramingLongLines reads lines of the maximum length made only of
// 0xAC bytes (twice as long once escaped), only of 0xFF bytes (the escape
// byte) and only of '€' in server mode, with the default OutputBufferMaxBytes
// and a MaxLineLength of 1 MiB. Before the escaped-frame fixes the 0xFF line
// took about a minute to escape and the 0xAC line was dropped silently with
// the lines around it; now server-mode output equals serverless output.
func TestUTF8FramingLongLines(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestUTF8FramingLongLines")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := context.WithTimeout(WithTestLogger(context.Background(), testLogger), 5*time.Minute)
	defer cancel()

	inFile := "dcat_utf8_long.tmp"
	writeLongLinesFile(t, inFile)
	cfgFile, env := longLinesConfig(t)
	serverlessColored := "dcat_utf8_long_serverless_colored.tmp"
	serverlessOut := "dcat_utf8_long_serverless.tmp"
	serverOut := "dcat_utf8_long_server.tmp"

	if _, err := runCommandWithEnv(ctx, t, serverlessColored, "../dcat", env,
		"--cfg", cfgFile, "--files", inFile); err != nil {
		t.Fatal(err)
	}
	writeWithoutColors(t, serverlessColored, serverlessOut)

	server := NewTestServer(t)
	if err := server.StartWithConfig(&ServerConfig{LogLevel: "error",
		ExtraArgs: []string{"--cfg", cfgFile}, Env: env}); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommandWithEnv(ctx, t, serverOut, "../dcat", env, "--cfg", cfgFile,
		"--servers", server.Address(), "--files", inFile, "--trustAllHosts", "--noColor"); err != nil {
		t.Fatal(err)
	}
	if err := compareFilesWithContext(ctx, t, serverOut, serverlessOut); err != nil {
		t.Fatalf("server-mode dcat output of long lines differs from serverless dcat: %v", err)
	}
}

// longLineLength is the MaxLineLength of TestUTF8FramingLongLines.
const longLineLength = 1024 * 1024

// writeLongLinesFile writes short lines around three lines of
// longLineLength bytes (with their newline).
func writeLongLinesFile(t *testing.T, file string) {
	t.Helper()
	var content strings.Builder
	content.WriteString("first line\n")
	for _, unit := range []string{"\xac", "\xff", "€"} {
		content.WriteString(strings.Repeat(unit, (longLineLength-1)/len(unit)))
		content.WriteString("\n")
	}
	content.WriteString("tail ¬ Ь line\n")
	if err := os.WriteFile(file, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// longLinesConfig writes a configuration with a MaxLineLength of
// longLineLength, the default OutputBufferMaxBytes and the integration
// paths, and returns it with the environment of its commands. The commands
// do not run in integration mode, which forces a MaxLineLength of 1024.
func longLinesConfig(t *testing.T) (string, map[string]string) {
	t.Helper()
	tmpDir := t.TempDir()
	content, err := json.Marshal(map[string]any{
		"Common": map[string]any{"HostnameOverride": "integrationtest"},
		"Server": map[string]any{
			"AuthorizedKeysPath":   os.Getenv("DTAIL_AUTH_KEY_PATH") + ".pub",
			"HostKeyPath":          suiteHostKeyPath(t),
			"MaxLineLength":        longLineLength,
			"OutputBufferMaxBytes": 2 * 1024 * 1024,
		},
		"Client": map[string]any{"KnownHostsPath": filepath.Join(tmpDir, "known_hosts")},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(tmpDir, "dtail.json")
	if err := os.WriteFile(cfgFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgFile, map[string]string{"DTAIL_INTEGRATION_TEST_RUN_MODE": "no"}
}
