package integrationtests

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// TestDMap4Append checks "outfile append PATH": a first query writes the
// outfile, two further queries append their rows to it (without a header, as
// the file is not empty) and keep the earlier content byte for byte. Each run
// writes the .query file of its own query. The append queries set a short
// interval, so that a run lasting longer than half of it gets interim
// reports, which must append nothing (an interim result of a cumulative query
// is the whole result so far); only the final result is appended.
// TestDTailQueryAppend covers a run spanning several report intervals.
//
// The test used to write "outfile append:PATH", which no release supported:
// it wrote a new file literally named "append:PATH" and the test passed
// because PATH stayed untouched. The parser now rejects such a path, which
// the last step checks.
func TestDMap4Append(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMap4Append")
	defer writeLogFileIgnoringError(testLogger)
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMap4Append",
		ServerlessTest: func(t *testing.T) { testDMap4AppendServerless(t, testLogger) },
		ServerTest:     func(t *testing.T) { testDMap4AppendWithServer(t, testLogger) },
	})
}

// dmapQueryRunner runs dmap with query, writing its stdout to stdout, and
// returns the exit status.
type dmapQueryRunner func(t *testing.T, stdout, query string) (int, error)

func testDMap4AppendServerless(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	run := func(t *testing.T, stdout, query string) (int, error) {
		ctx, cancel := createTestContextWithTimeout(t)
		defer cancel()
		return runCommand(ctx, t, stdout,
			"../dmap", "--query", query, "--cfg", "none", paths.MaprTestData)
	}
	runDMap4AppendSteps(ctx, t, "dmap4_serverless", run)
}

func testDMap4AppendWithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}

	baseArgs := NewCommandArgs()
	baseArgs.Servers = []string{server.Address()}
	baseArgs.TrustAllHosts = true
	baseArgs.NoColor = true
	baseArgs.Files = []string{paths.MaprTestData}
	run := func(t *testing.T, stdout, query string) (int, error) {
		args := *baseArgs
		args.ExtraArgs = []string{"--query", query}
		return runCommand(server.ctx, t, stdout, "../dmap", args.ToSlice()...)
	}
	runDMap4AppendSteps(ctx, t, "dmap4_server", run)
}

// runDMap4AppendSteps runs the steps of TestDMap4Append with the outfile
// <prefix>.csv.tmp. Each step depends on the outfile the previous one left.
func runDMap4AppendSteps(ctx context.Context, t *testing.T, prefix string, run dmapQueryRunner) {
	csvFile := prefix + ".csv.tmp"
	queryFile := csvFile + ".query"
	cleanupFiles(t, csvFile, queryFile)

	first := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)
	second := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time rorder by avg($goroutines) "+
		"interval 1 outfile append %s", csvFile)
	third := fmt.Sprintf("from STATS select count($line),$hostname "+
		"group by $hostname interval 1 outfile append %s", csvFile)

	t.Run("FirstQuery", func(t *testing.T) {
		mustRunDMap(t, run, prefix+".stdout1.tmp", first)
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}
		if err := verifyQueryFile(t, queryFile, first); err != nil {
			t.Error(err)
		}
	})
	// The rows of the first query (in another order) again, without the
	// header, then the one row of the third query ($hostname is the
	// HostnameOverride of the integration test mode, in both modes).
	steps := []struct {
		name, query string
		rows        []string
	}{
		{"SecondQueryWithAppend", second, csvLines(mustReadFile(t, "dmap4_query1.csv.expected"))[1:]},
		{"ThirdQueryWithAppend", third, []string{"597,integrationtest"}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			before := mustReadFile(t, csvFile)
			mustRunDMap(t, run, prefix+"."+step.name+".stdout.tmp", step.query)
			verifyAppendedRows(t, csvFile, before, step.rows)
			if err := verifyQueryFile(t, queryFile, step.query); err != nil {
				t.Error(err)
			}
		})
	}
	t.Run("AppendColonPathRejected", func(t *testing.T) {
		testDMap4AppendColonRejected(t, run, prefix, csvFile)
	})
}

// testDMap4AppendColonRejected checks that "outfile append:PATH" fails
// before anything is written: PATH keeps its content and no file named
// "append:PATH" appears.
func testDMap4AppendColonRejected(t *testing.T, run dmapQueryRunner, prefix, csvFile string) {
	colonFile := "append:" + csvFile
	cleanupFiles(t, colonFile, colonFile+".query")
	before := mustReadFile(t, csvFile)
	query := fmt.Sprintf("from STATS select count($line),$hostname "+
		"group by $hostname outfile %s", colonFile)
	stdout := prefix + ".colon.stdout.tmp"
	cleanupFiles(t, stdout)
	if status, _ := run(t, stdout, query); status == 0 {
		t.Errorf("dmap with outfile %q exited 0, want a non-zero status", colonFile)
	}
	if !bytes.Equal(mustReadFile(t, csvFile), before) {
		t.Errorf("rejected query changed %s", csvFile)
	}
	if _, err := os.Stat(colonFile); !os.IsNotExist(err) {
		t.Errorf("rejected query created %s (stat error %v)", colonFile, err)
	}
}

// mustRunDMap runs query and fails the test unless dmap exits 0.
func mustRunDMap(t *testing.T, run dmapQueryRunner, stdout, query string) {
	t.Helper()
	cleanupFiles(t, stdout)
	status, err := run(t, stdout, query)
	if err != nil || status != 0 {
		t.Fatalf("dmap --query %q: status %d, error %v", query, status, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// verifyAppendedRows checks that csvFile still starts with before, byte for
// byte, and that what follows are the rows want, in any order (and no
// header, as a result appended to a non-empty file has none).
func verifyAppendedRows(t *testing.T, csvFile string, before []byte, want []string) {
	t.Helper()
	after := mustReadFile(t, csvFile)
	if !bytes.HasPrefix(after, before) {
		t.Fatalf("%s does not keep its earlier %d bytes", csvFile, len(before))
	}
	got := csvLines(after[len(before):])
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: appended rows %q, want %q", csvFile, got, want)
	}
}

// csvLines splits content into its lines, without a last empty one.
func csvLines(content []byte) []string {
	text := strings.TrimSuffix(string(content), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestDTailQueryAppend follows a growing file with "dtail --query ... outfile
// append PATH" for several report intervals (interval 1, --shutdownAfter 5).
// The query is cumulative (it has an outfile), so every interim report has the
// whole result so far: those must append nothing, and PATH must get the final
// result exactly once, here the CSV header and the one $hostname row.
func TestDTailQueryAppend(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDTailQueryAppend")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := context.WithCancel(WithTestLogger(context.Background(), testLogger))
	defer cancel()

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Fatal(err)
	}
	followFile := "dtail_query_append.follow.tmp"
	csvFile := "dtail_query_append.csv.tmp"
	stdout := "dtail_query_append.stdout.tmp"
	cleanupFiles(t, followFile, csvFile, csvFile+".query", stdout)
	writeStatsLines(ctx, t, followFile, mustReadFile(t, GetStandardTestPaths().MaprTestData))

	query := fmt.Sprintf("from STATS select count($line),$hostname group by $hostname "+
		"interval 1 outfile append %s", csvFile)
	status, err := runCommand(ctx, t, stdout, "../dtail",
		"--cfg", "none", "--logger", "stdout", "--logLevel", "error",
		"--servers", server.Address(), "--files", followFile,
		"--trustAllHosts", "--noColor", "--shutdownAfter", "5", "--query", query)
	if err != nil || status != 0 {
		t.Fatalf("dtail --query %q: status %d, error %v", query, status, err)
	}

	lines := csvLines(mustReadFile(t, csvFile))
	if len(lines) != 2 || lines[0] != "count($line),$hostname" ||
		!regexp.MustCompile(`^[1-9][0-9]*,integrationtest$`).MatchString(lines[1]) {
		t.Fatalf("%s = %q, want the header and exactly one result row", csvFile, lines)
	}
	if err := verifyQueryFile(t, csvFile+".query", query); err != nil {
		t.Error(err)
	}
}

// writeStatsLines creates path and appends the first line of data to it
// every 100 ms until ctx is done, so that a follow read gets new lines in
// every report interval.
func writeStatsLines(ctx context.Context, t *testing.T, path string, data []byte) {
	t.Helper()
	line, _, _ := bytes.Cut(data, []byte("\n"))
	line = append(line, '\n')
	fd, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		<-done
		_ = fd.Close()
	})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = fd.Write(line)
			case <-ctx.Done():
				return
			}
		}
	}()
}
