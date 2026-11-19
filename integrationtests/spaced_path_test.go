package integrationtests

// Integration tests for read targets whose path contains a space. dserver
// splits every command on single spaces, so such a path used to be cut at
// its first space and the rest parsed as the regex ("Unable to parse
// command"), and the file was never read. Clients now encode such a path
// (protocol.EncodeReadPath) and dserver decodes it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	spacedPathContent = "spaced alpha line\nspaced beta line\n"
	// spacedPathDecoy is the content of the file named like the spaced
	// path's part before its first space, which a server splitting the path
	// would get as its path.
	spacedPathDecoy = "decoy line of the wrong file\n"
)

// spacedPathFixture is a directory with spaces holding a file with a spaced
// name and a decoy file named like that name's part before its first space.
type spacedPathFixture struct {
	dir   string
	file  string
	decoy string
}

func newSpacedPathFixture(t *testing.T) spacedPathFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dir with  spaces")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := spacedPathFixture{
		dir:   dir,
		file:  filepath.Join(dir, "app.log rotated.log"),
		decoy: filepath.Join(dir, "app.log"),
	}
	for path, content := range map[string]string{fixture.file: spacedPathContent, fixture.decoy: spacedPathDecoy} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// spacedPathModes returns the client arguments of serverless mode (none) and
// of server mode against a fresh dserver.
func spacedPathModes(t *testing.T) map[string][]string {
	t.Helper()
	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}
	return map[string][]string{
		"serverless": nil,
		"server":     {"--servers", server.Address(), "--trustAllHosts", "--noColor"},
	}
}

// TestSpacedPathDCatDGrep reads a spaced path with dcat (--files and
// positional) and a spaced glob with dgrep, serverless and against dserver.
func TestSpacedPathDCatDGrep(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestSpacedPathDCatDGrep")
	defer writeLogFileIgnoringError(testLogger)
	ctx := WithTestLogger(context.Background(), testLogger)
	fixture := newSpacedPathFixture(t)

	for mode, serverArgs := range spacedPathModes(t) {
		cases := []struct {
			name, cmd, want string
			args            []string
		}{
			{"dcat files", "../dcat", spacedPathContent, []string{"--files", fixture.file}},
			{"dcat positional", "../dcat", spacedPathContent, []string{fixture.file}},
			{"dgrep glob", "../dgrep", "spaced beta line\n",
				[]string{"--regex", "beta", "--files", filepath.Join(fixture.dir, "* *.log")}},
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				outFile := "spaced_path.stdout.tmp"
				cleanupFiles(t, outFile)
				args := append(append([]string{"--plain", "--cfg", "none"}, serverArgs...), tc.args...)
				if _, err := runCommand(ctx, t, outFile, tc.cmd, args...); err != nil {
					t.Fatalf("%s failed: %v\n%s", tc.cmd, err, readTestFile(t, outFile))
				}
				if got := readTestFile(t, outFile); got != tc.want {
					t.Fatalf("%s printed %q, want %q", tc.cmd, got, tc.want)
				}
			})
		}
	}
}

// TestSpacedPathMissingFileReadsNoOtherFile asks for a spaced path that does
// not exist while its part before the first space does: the client must not
// print the decoy, the server must report the whole spaced path as missing
// ("No such file(s) to read|<path>", printed by the serverless client and
// logged by dserver, which sends the client no path), and the exit status
// must be that of a missing path without spaces: 2, as the server reports
// the missing file with ".syn command failed read: no file to read" (task
// fe3), which serverless mode's in-process server does the same way.
func TestSpacedPathMissingFileReadsNoOtherFile(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestSpacedPathMissingFileReadsNoOtherFile")
	defer writeLogFileIgnoringError(testLogger)
	ctx := WithTestLogger(context.Background(), testLogger)
	fixture := newSpacedPathFixture(t)
	missing := fixture.decoy + " missing.log"
	missingPlain := filepath.Join(t.TempDir(), "missing.log")
	const noFile = "No such file(s) to read|"
	// A dserver logging at info, whose log holds the no-file reports.
	server := startSharedReadServer(t, sharedReadModes()[0], nil)
	modes := map[string][]string{
		"serverless": nil,
		"server":     {"--servers", server.address, "--trustAllHosts", "--noColor"},
	}

	for mode, serverArgs := range modes {
		t.Run(mode, func(t *testing.T) {
			outFile := "spaced_path_missing.stdout.tmp"
			cleanupFiles(t, outFile)
			args := append([]string{"--plain", "--cfg", "none"}, serverArgs...)
			wantStatus, _ := runCommand(ctx, t, outFile, "../dcat", append(args, "--files", missingPlain)...)
			// Pin the reference status too, so the comparison below cannot
			// pass with both runs exiting 0 if the failure report regressed.
			if wantStatus != 2 {
				t.Fatalf("dcat of the missing %q exited with %d, want 2", missingPlain, wantStatus)
			}
			status, _ := runCommand(ctx, t, outFile, "../dcat", append(args, "--files", missing)...)
			got := readTestFile(t, outFile)
			if status != wantStatus {
				t.Fatalf("dcat of the missing %q exited with %d, a missing path without spaces with %d:\n%s",
					missing, status, wantStatus, got)
			}
			if serverArgs == nil && !strings.Contains(got, noFile+missing+"\n") {
				t.Fatalf("dcat of %q did not report the whole path as missing:\n%s", missing, got)
			}
			if serverArgs != nil {
				server.waitForLog(t, noFile+missing, 1)
			}
			if strings.Contains(got, strings.TrimSpace(spacedPathDecoy)) {
				t.Fatalf("dcat of the missing %q read the decoy file:\n%s", missing, got)
			}
			if strings.Contains(got, "Unable to parse command") {
				t.Fatalf("dcat of %q failed to parse the command:\n%s", missing, got)
			}
		})
	}
	server.requireLogCount(t, "Unable to parse command", 0)
}

// TestSpacedPathDMap runs a mapreduce query over a spaced copy of the
// mapreduce test data, serverless and against dserver, and expects the
// result of the original file.
func TestSpacedPathDMap(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestSpacedPathDMap")
	defer writeLogFileIgnoringError(testLogger)
	ctx := WithTestLogger(context.Background(), testLogger)
	input := copySpacedMaprTestData(t)

	for mode, serverArgs := range spacedPathModes(t) {
		t.Run(mode, func(t *testing.T) {
			csvFile := "spaced_path_dmap.csv.tmp"
			outFile := "spaced_path_dmap.stdout.tmp"
			cleanupFiles(t, csvFile, csvFile+".query", outFile)
			query := "from STATS select count($line),last($time)," +
				"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
				"group by $hostname outfile " + csvFile
			args := append(append([]string{"--cfg", "none", "--logger", "stdout", "--logLevel", "error",
				"--noColor", "--query", query}, serverArgs...), "--files", input)
			if _, err := runCommand(ctx, t, outFile, "../dmap", args...); err != nil {
				t.Fatalf("dmap failed: %v\n%s", err, readTestFile(t, outFile))
			}
			if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap1a.csv.expected"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func copySpacedMaprTestData(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(GetStandardTestPaths().MaprTestData)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(newSpacedPathFixture(t).dir, "mapr test data.log")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSpacedPathDTailFollow follows a spaced path with dtail, serverless and
// against dserver, and expects the lines appended to it.
func TestSpacedPathDTailFollow(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestSpacedPathDTailFollow")
	defer writeLogFileIgnoringError(testLogger)
	fixture := newSpacedPathFixture(t)

	for mode, serverArgs := range spacedPathModes(t) {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := createTestContextWithTimeout(t)
			defer cancel()
			ctx = WithTestLogger(ctx, testLogger)
			args := append(append([]string{"--cfg", "none", "--logger", "stdout", "--logLevel", "info",
				"--noColor", "--plain"}, serverArgs...), "--files", fixture.file)
			stdoutCh, stderrCh, _, err := startCommand(ctx, t, "", "../dtail", args...)
			if err != nil {
				t.Fatal(err)
			}
			output := startProcessOutputCollector(ctx, stdoutCh, stderrCh)
			needle := "spaced follow line " + mode
			for i := 0; countSubstring(output.snapshot(), needle) == 0; i++ {
				if countSubstring(output.snapshot(), "Unable to parse command") > 0 {
					t.Fatalf("dtail failed to parse the command:\n%s", output.dump())
				}
				if err := appendLines(fixture.file, fmt.Sprintf("%s %d", needle, i)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatalf("dtail did not follow %q:\n%s", fixture.file, output.dump())
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestSpacedPathInteractiveReload starts an interactive dgrep (SESSION
// START, Files in the JSON spec) on a spaced path and reloads it to another
// spaced path given quoted to :reload --files (SESSION UPDATE).
func TestSpacedPathInteractiveReload(t *testing.T) {
	skipIfNotIntegrationTest(t)
	testLogger := NewTestLogger("TestSpacedPathInteractiveReload")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	fixture := newSpacedPathFixture(t)
	second := filepath.Join(fixture.dir, "second file.log")
	if err := os.WriteFile(second, []byte("second spaced gamma line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	port, _ := startInteractiveTestServer(ctx, t)
	clientOutput, err := runInteractivePTYCommand(ctx, []string{
		"../dgrep", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port), "--files", fixture.file,
		"--grep", "line", "--plain", "--trustAllHosts", "--interactive-query",
	}, []interactiveStep{
		{WaitFor: "spaced beta line", Input: ":reload --files '" + second + "'"},
		{WaitFor: "second spaced gamma line", Input: ":quit"},
	})
	if err != nil {
		t.Fatalf("run interactive dgrep: %v\noutput:\n%s", err, clientOutput)
	}
	for _, needle := range []string{"spaced alpha line", "reload applied successfully", "second spaced gamma line"} {
		if !strings.Contains(clientOutput, needle) {
			t.Fatalf("expected %q in output:\n%s", needle, clientOutput)
		}
	}
	for _, needle := range []string{"Unable to parse command", strings.TrimSpace(spacedPathDecoy)} {
		if strings.Contains(clientOutput, needle) {
			t.Fatalf("unexpected %q in output:\n%s", needle, clientOutput)
		}
	}
}

// TestSpacedPathScheduledJob runs a scheduled job whose Files is a spaced
// path and expects the outfile of the original mapreduce test data.
func TestSpacedPathScheduledJob(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestSpacedPathScheduledJob")
	defer writeLogFileIgnoringError(testLogger)

	csvFile := "spaced_path_schedule.csv.tmp"
	cfgFile := "spaced_path_schedule.cfg.tmp"
	cleanupFiles(t, csvFile, csvFile+".query", cfgFile)
	input := copySpacedMaprTestData(t)
	cfg := fmt.Sprintf(`{"Server": {"Schedule": [{"Name": "spaced_path_schedule", "Enable": true,
		"AllowFrom": ["localhost"], "TimeRange": [0, 24], "Files": %q,
		"Query": "from STATS select count($line),last($time),avg($goroutines),min(concurrentConnections),max(lifetimeConnections) group by $hostname",
		"Outfile": %q}]}}`, input, "./"+csvFile)
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(WithTestLogger(context.Background(), testLogger))
	defer cancel()
	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t, "", "../dserver",
		"--cfg", cfgFile, "--logger", "stdout", "--logLevel", "info",
		"--bindAddress", "127.0.0.1", "--shutdownAfter", "10",
		"--port", fmt.Sprintf("%d", getUniquePortNumber()),
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFilesWithContext(ctx, t, csvFile, "dserver1.csv.expected"); err != nil {
		t.Fatal(err)
	}
}

// TestSpacedPathScheduledGroup runs two scheduled jobs with the same spaced
// Files. The scheduler groups them (option share=<group>:<members>), so
// dserver reads the spaced path once for both through a shared one-shot read
// (readhub), and each outfile must equal that of the job run on its own.
func TestSpacedPathScheduledGroup(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	input := copySpacedMaprTestData(t)
	const query = "from STATS select count($line),last($time),avg($goroutines)," +
		"min(concurrentConnections),max(lifetimeConnections) group by $hostname"
	jobs := map[string]struct{ where, expected string }{
		"spaced_group_a": {"", "dserver3a.csv.expected"},
		"spaced_group_b": {" where lifetimeConnections >= 3", "dserver3b.csv.expected"},
	}
	var schedule []map[string]any
	for name, job := range jobs {
		cleanupFiles(t, name+".csv.tmp", name+".csv.tmp.query")
		schedule = append(schedule, map[string]any{
			"Name": name, "Enable": true, "AllowFrom": []string{"localhost"},
			"TimeRange": []int{0, 24}, "Files": input, "Query": query + job.where,
			"Outfile": "./" + name + ".csv.tmp",
		})
	}

	server := startSharedReadServer(t, sharedReadModes()[0], map[string]any{
		"MaxConcurrentCats": 4, "MaxConnections": 12, "Schedule": schedule,
	})
	for name := range jobs {
		server.waitForLog(t, fmt.Sprintf("|Job %s exited with status 0", name), 1)
	}
	for name, job := range jobs {
		if err := compareFiles(t, name+".csv.tmp", job.expected); err != nil {
			t.Error(err)
		}
	}
	server.requireLogCount(t, "|Starting job group of 2 jobs reading", 1)
	server.requireLogCount(t, sharedOneShotStartedLog, 1)
	// The read was shared by both members (the second member's join and the
	// start of the read log members=2/2), and it read the decoded spaced
	// path: the hub logs the member's resolved path before the message.
	server.requireLogCount(t, "|members=2/2", 2)
	resolved, err := filepath.EvalSymlinks(input)
	if err != nil {
		t.Fatal(err)
	}
	server.requireLogCount(t, "|"+resolved+sharedOneShotStartedLog, 1)
	server.requireLogCount(t, "Unable to parse command", 0)
}

// TestSpacedPathSharedFollow follows a spaced path with two dtail clients,
// which with sharing on share one reader of it (readhub), and with sharing
// off read privately; both must print every matching line appended to it,
// the same in both modes.
func TestSpacedPathSharedFollow(t *testing.T) {
	const regex = "foo|SYNC|MARK"
	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		file := newSpacedPathFixture(t).file
		server := startSharedReadServer(t, mode, nil)
		first := startFollowClient(t, server, "spaced_first", file, "--plain", "--grep", regex)
		second := startFollowClient(t, server, "spaced_second", file, "--plain", "--grep", regex)
		if mode.shared() {
			server.waitForLog(t, sharedFollowGainedLog+"2", 1)
		} else {
			server.waitForReaders(t, file, 2)
		}
		written := syncFollowClients(t, file, first, second)
		marked := markedLines("MARK", 100)
		appendFileLines(t, file, marked...)
		written = append(written, marked...)
		waitForClients(t, marked[len(marked)-1], first, second)

		want := matchingLines(t, regex, written)
		outputs := map[string]string{}
		for _, client := range []*followClient{first, second} {
			outputs[client.name] = client.outputWithoutSync(t)
			if outputs[client.name] != want {
				t.Errorf("dtail %s output differs from the matching lines:\n%s",
					client.name, firstDifference(outputs[client.name], want))
			}
		}
		if mode.shared() {
			server.requireLogCount(t, sharedFollowStartedLog, 1)
		} else {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		server.requireLogCount(t, "Unable to parse command", 0)
		return outputs
	})
}
