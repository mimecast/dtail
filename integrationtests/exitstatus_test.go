package integrationtests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// exitStatusLine is the content of the readable files of the exit status
// tests.
const exitStatusLine = "exit status test line"

// reportedFailureLog is in the client's debug log when the server reported a
// failed command (see handlers.baseHandler.handleCommandFailedMessage).
const reportedFailureLog = "Server reported a failed command"

// exitStatusFiles are the files the exit status tests read.
type exitStatusFiles struct {
	readable   string
	other      string // readable too, but not permitted by the restricted server
	missing    string
	unreadable string // exists but has no read permission on the file system
}

// exitStatusCase is one dcat or dgrep run and the exit status it must end
// with; wantOutput, if set, must be in its output. The run uses --plain
// unless framed is set, which keeps the default framed output (in server
// mode REMOTE|... payload lines, hidden messages unambiguous from payload).
type exitStatusCase struct {
	name       string
	cmd        string
	args       []string
	want       int
	wantOutput string
	framed     bool
}

// TestDCatDGrepExitStatus checks that dcat and dgrep exit with status 2 when
// the server (or the in-process server in serverless mode) reported that
// files could not be read, and with status 0 otherwise, also when dgrep
// matched no line.
func TestDCatDGrepExitStatus(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDCatDGrepExitStatus")
	defer writeLogFileIgnoringError(testLogger)
	files := newExitStatusFiles(t)
	cases := exitStatusCases(t, files)

	t.Run("Serverless", func(t *testing.T) {
		runExitStatusCases(t, testLogger, cases, nil)
	})
	t.Run("ServerMode", func(t *testing.T) {
		server := NewTestServer(t)
		if err := server.Start("error"); err != nil {
			t.Fatalf("start dserver: %v", err)
		}
		runExitStatusCases(t, testLogger, cases,
			[]string{"--servers", server.Address(), "--trustAllHosts"})
	})
	t.Run("ServerModePermissionDenied", func(t *testing.T) {
		testExitStatusPermissionDenied(t, testLogger, files)
	})
}

// testExitStatusPermissionDenied runs dcat against a dserver whose read
// permissions allow only files.readable.
func testExitStatusPermissionDenied(t *testing.T, logger *TestLogger, files exitStatusFiles) {
	configFile := writeTurboFileServerConfig(t, files.readable)
	server := startDJournalExtendedServer(t, logger, configFile,
		map[string]string{"DTAIL_HOSTNAME_OVERRIDE": "integrationtest"}, "error", true)
	runExitStatusCases(t, logger, []exitStatusCase{
		{name: "dcat permitted file", cmd: "../dcat", args: []string{"--files", files.readable},
			want: 0, wantOutput: exitStatusLine},
		{name: "dcat not permitted file", cmd: "../dcat", args: []string{"--files", files.other}, want: 2},
		{name: "dgrep not permitted file", cmd: "../dgrep",
			args: []string{"--grep", "exit", "--files", files.other}, want: 2},
	}, []string{"--servers", server.address, "--trustAllHosts"})
}

func newExitStatusFiles(t *testing.T) exitStatusFiles {
	t.Helper()
	dir := t.TempDir()
	files := exitStatusFiles{
		readable:   filepath.Join(dir, "readable.log"),
		other:      filepath.Join(dir, "other.log"),
		missing:    filepath.Join(dir, "missing.log"),
		unreadable: filepath.Join(dir, "unreadable.log"),
	}
	for _, path := range []string{files.readable, files.other} {
		if err := os.WriteFile(path, []byte(exitStatusLine+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.WriteFile(files.unreadable, []byte(exitStatusLine+"\n"), 0o000); err != nil {
		t.Fatalf("write %s: %v", files.unreadable, err)
	}
	return files
}

// exitStatusCases are the runs of the serverless and server mode tests.
func exitStatusCases(t *testing.T, files exitStatusFiles) []exitStatusCase {
	cases := []exitStatusCase{
		{name: "dcat readable file", cmd: "../dcat", args: []string{"--files", files.readable},
			want: 0, wantOutput: exitStatusLine},
		{name: "dcat missing file", cmd: "../dcat", args: []string{"--files", files.missing}, want: 2},
		{name: "dcat readable and missing file", cmd: "../dcat",
			args: []string{"--files", files.readable + "," + files.missing}, want: 2, wantOutput: exitStatusLine},
		{name: "dcat readable and missing file, framed output", cmd: "../dcat", framed: true,
			args: []string{"--files", files.readable + "," + files.missing}, want: 2, wantOutput: exitStatusLine},
		{name: "dgrep match", cmd: "../dgrep", args: []string{"--grep", "exit", "--files", files.readable},
			want: 0, wantOutput: exitStatusLine},
		{name: "dgrep no match", cmd: "../dgrep",
			args: []string{"--grep", "nothing-matches-this", "--files", files.readable}, want: 0},
		{name: "dgrep missing file", cmd: "../dgrep",
			args: []string{"--grep", "exit", "--files", files.missing}, want: 2},
	}
	// root reads a file without read permission all the same.
	if os.Geteuid() != 0 {
		cases = append(cases, exitStatusCase{name: "dcat unreadable file", cmd: "../dcat",
			args: []string{"--files", files.unreadable}, want: 2})
	} else {
		t.Log("Not checking an unreadable file as root")
	}
	return cases
}

// runExitStatusCases runs each case with the common flags (--plain unless
// the case is framed) and modeArgs (none for serverless mode) and checks its
// exit status and output.
func runExitStatusCases(t *testing.T, logger *TestLogger, cases []exitStatusCase, modeArgs []string) {
	t.Helper()
	ctx := WithTestLogger(context.Background(), logger)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outFile := fmt.Sprintf("exitstatus_%d.stdout.tmp", i)
			args := []string{"--cfg", "none", "--logLevel", "error", "--noColor"}
			if !tc.framed {
				args = append([]string{"--plain"}, args...)
			}
			args = append(args, modeArgs...)
			args = append(args, tc.args...)
			exitCode, err := runCommand(ctx, t, outFile, tc.cmd, args...)
			output := readTestFile(t, outFile)
			if exitCode != tc.want {
				t.Fatalf("%s exit status = %d (%v), want %d; output:\n%s", tc.cmd, exitCode, err, tc.want, output)
			}
			if tc.wantOutput != "" && !strings.Contains(output, tc.wantOutput) {
				t.Fatalf("%s output misses %q:\n%s", tc.cmd, tc.wantOutput, output)
			}
		})
	}
}

// TestDTailExitStatusUnaffected checks that dtail's follow mode keeps its
// exit status 0 when the server reported that a file could not be read: only
// dcat and dgrep exit with status 2 then. The client's debug log shows that
// the server did report the failure.
func TestDTailExitStatusUnaffected(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDTailExitStatusUnaffected")
	defer writeLogFileIgnoringError(testLogger)
	files := newExitStatusFiles(t)
	dtailCase := func(name, file string) exitStatusCase {
		return exitStatusCase{name: name, cmd: "../dtail", want: 0, wantOutput: reportedFailureLog,
			args: []string{"--logLevel", "debug", "--shutdownAfter", "3", "--files", file}}
	}

	if os.Geteuid() != 0 {
		cases := []exitStatusCase{dtailCase("dtail unreadable file", files.unreadable)}
		t.Run("Serverless", func(t *testing.T) {
			runExitStatusCases(t, testLogger, cases, nil)
		})
		t.Run("ServerMode", func(t *testing.T) {
			server := NewTestServer(t)
			if err := server.Start("error"); err != nil {
				t.Fatalf("start dserver: %v", err)
			}
			runExitStatusCases(t, testLogger, cases,
				[]string{"--servers", server.Address(), "--trustAllHosts"})
		})
	}
	t.Run("ServerModePermissionDenied", func(t *testing.T) {
		configFile := writeTurboFileServerConfig(t, files.readable)
		server := startDJournalExtendedServer(t, testLogger, configFile,
			map[string]string{"DTAIL_HOSTNAME_OVERRIDE": "integrationtest"}, "error", true)
		runExitStatusCases(t, testLogger, []exitStatusCase{dtailCase("dtail not permitted file", files.other)},
			[]string{"--servers", server.address, "--trustAllHosts"})
	})
}
