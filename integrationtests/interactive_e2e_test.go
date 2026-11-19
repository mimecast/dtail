package integrationtests

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// e2eStatsLog and e2eKVLog are the logs of the interactive end-to-end
// tests: MAPREDUCE:STATS lines of the default log format and generickv
// lines (field=value|...).
const (
	e2eStatsLog = "interactive_e2e_stats.log.tmp"
	e2eKVLog    = "interactive_e2e_kv.log.tmp"
)

// writeE2ELogs writes the two logs: 30 STATS lines with three values of
// currentConnections and status OKAY, except every fifth line FAIL (two per
// currentConnections value), and generickv lines.
func writeE2ELogs(t *testing.T) {
	t.Helper()
	var stats, kv strings.Builder
	for i := range 30 {
		fmt.Fprintf(&stats, "INFO|1118-120000|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
			"currentConnections=%d|bytes=%d|status=%s\n", i%3, i, map[bool]string{true: "FAIL", false: "OKAY"}[i%5 == 0])
		fmt.Fprintf(&kv, "user=user%d|action=login|took=%d\n", i%2, i)
	}
	for path, content := range map[string]string{e2eStatsLog: stats.String(), e2eKVLog: kv.String()} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cleanupFiles(t, e2eStatsLog, e2eKVLog)
}

// TestInteractiveQueryCompletionEndToEnd runs an interactive dmap on a PTY
// against dserver and serverless: once the background probe answered,
// :fields lists the fields of both logs (the default log format's STATS
// table and the generickv fields), and a query is typed with Tab
// completing a clause keyword, the table, an aggregation, a field inside
// it, a where field, an operator and a sample value the probe found. The
// applied query is echoed, and its result table holds the expected counts.
func TestInteractiveQueryCompletionEndToEnd(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testInteractiveQueryCompletionEndToEnd(t, serverless)
		})
	}
}

func testInteractiveQueryCompletionEndToEnd(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestInteractiveQueryCompletionEndToEnd")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()
	writeE2ELogs(t)

	argv := []string{"../dmap", "--cfg", "none", "--logger", "stdout", "--logLevel", "debug",
		"--files", e2eStatsLog + "," + e2eKVLog, "--plain", "--interactive-query",
		"--query", "from STATS select count($line) group by $hostname"}
	if !serverless {
		port, _ := startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port), "--trustAllHosts")
	}
	steps, line := queryCompletionSteps()
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run interactive dmap: %v\noutput:\n%s", err, output)
	}
	for _, needle := range []string{"table STATS:", "log format generickv:", "took", "user0",
		line, "reload applied successfully", `query: from STATS select count(bytes) where status eq "OKAY"`} {
		if !strings.Contains(output, needle) {
			t.Fatalf("output lacks %q:\n%s", needle, output)
		}
	}
	// The result of the reloaded query: 8 lines with status OKAY per value
	// of currentConnections (10 without the where clause).
	if !queryResultTable.MatchString(output) {
		t.Fatalf("no result table of the reloaded query:\n%s", output)
	}
}

// queryResultTable is the result table of the query the end-to-end test
// types: its header, the dashes and 8 per value of currentConnections.
var queryResultTable = regexp.MustCompile(`count\(bytes\) *\r?\n-+\r?\n *8 *\r?\n *8 *\r?\n *8 *\r?\n`)

// queryCompletionSteps returns the PTY steps of the end-to-end test and the
// line typed up to the where value. Each Tab completes one word; the wait
// is for the line up to the cursor as the editor draws it again after the
// completion, which stays within the PTY's 80 columns (a wrapped line is
// not drawn as one piece; only the typed group by clause wraps, and
// nothing waits for it). The last step waits for the reloaded result table
// (its header followed by the dashes, which neither the echo nor the raw
// query line has) before :quit, as the result arrives after the echo.
func queryCompletionSteps() ([]interactiveStep, string) {
	reload := ":reload --query '"
	steps := []interactiveStep{
		{WaitFor: probeStoredMessage, Input: ":fields"},
		{WaitFor: "log format generickv:", Input: "\n" + reload + "fr\t", Raw: true},
	}
	line := reload + "from "
	for _, tab := range []struct{ typed, completed string }{
		{"ST", "STATS "}, {"sel", "select "}, {"cou", "count("}, {"byt", "bytes)"},
		{" whe", " where "}, {"sta", "status "}, {"e", "eq "}, {`"O`, `"OKAY" `},
	} {
		steps = append(steps, interactiveStep{WaitFor: line, Input: tab.typed + "\t", Raw: true})
		line += tab.completed
	}
	return append(steps,
		interactiveStep{WaitFor: line, Input: "group by currentConnections'"},
		interactiveStep{WaitFor: "count(bytes) \r\n---", Input: ":quit"},
	), line
}

// TestInteractiveControlColors runs dtail on a PTY with terminal colors
// (no --plain, no --noColor) and with --noColor: with colors the banner and
// the :show answer are painted, without them the control output carries no
// SGR sequence at all (x/term still moves the cursor with other sequences).
func TestInteractiveControlColors(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, noColor := range []bool{false, true} {
		t.Run(fmt.Sprintf("noColor=%v", noColor), func(t *testing.T) {
			testInteractiveControlColors(t, noColor)
		})
	}
}

func testInteractiveControlColors(t *testing.T, noColor bool) {
	testLogger := NewTestLogger("TestInteractiveControlColors")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()
	writeE2ELogs(t)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--files", e2eStatsLog, "--grep", "NOTHING-MATCHES", "--interactive-query"}
	if noColor {
		argv = append(argv, "--noColor")
	}
	steps := []interactiveStep{
		{WaitFor: "Tab completes", Input: ":show"},
		// invert is a key of :show only (the client's STATS lines have
		// servers= too).
		{WaitFor: "invert", Input: ":quit"},
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	sgr := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	if noColor {
		if found := sgr.FindString(output); found != "" {
			t.Fatalf("--noColor output has the SGR sequence %q:\n%q", found, output)
		}
		if !strings.Contains(output, "invert=false") {
			t.Fatalf("no :show answer:\n%q", output)
		}
		return
	}
	// The default theme: banner cyan bold, :show keys cyan.
	for _, want := range []string{"\x1b[36m\x1b[1mInteractive query control",
		"\x1b[36minvert\x1b[0m\x1b[39m="} {
		if !strings.Contains(output, want) {
			t.Fatalf("colored output lacks %q:\n%q", want, output)
		}
	}
}

// TestInteractiveTabOffersOnlyPermittedPaths completes a --files value
// where the read permissions allow only one file of a directory: against a
// dserver with them in its config, and serverless with them in the
// client's config (the in-process server reads its Server section). Tab
// offers (and so completes) that file alone, never the other one, whose
// name must not reach the client.
func TestInteractiveTabOffersOnlyPermittedPaths(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testInteractiveTabOffersOnlyPermittedPaths(t, serverless)
		})
	}
}

func testInteractiveTabOffersOnlyPermittedPaths(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestInteractiveTabOffersOnlyPermittedPaths")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	dir, readable := writePermPathsDir(t)
	configFile := writeTurboFileServerConfig(t, readable)
	argv := []string{"../dtail", "--logger", "stdout", "--logLevel", "debug", "--files", readable,
		"--grep", "MARK", "--plain", "--interactive-query"}
	if serverless {
		argv = append(argv, "--cfg", configFile)
	} else {
		server := startDJournalExtendedServer(t, testLogger, configFile,
			map[string]string{"DTAIL_HOSTNAME_OVERRIDE": "integrationtest"}, "info", true)
		argv = append(argv, "--cfg", "none", "--servers", server.address, "--trustAllHosts")
	}
	steps := []interactiveStep{
		{WaitFor: pathListingStoredMessage, Input: ":reload --files " + dir + "/\t", Raw: true},
		{WaitFor: ":reload --files " + dir + "/permitted.log", Input: "\x15:quit"},
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	if !strings.Contains(output, ":reload --files "+dir+"/permitted.log") {
		t.Fatalf("the permitted file was not completed:\n%s", output)
	}
	// Serverless, the in-process server logs its permission checks to the
	// client's output (SERVER| lines naming secret.log as denied); any other
	// line naming it, a candidate list included, is a leak.
	for _, outputLine := range strings.Split(output, "\n") {
		if strings.Contains(outputLine, "secret") && !strings.Contains(outputLine, "SERVER|") {
			t.Fatalf("a file the user may not read reached the client: %q\n%s", outputLine, output)
		}
	}
}

// writePermPathsDir writes the directory of the permission test, afresh:
// permitted.log and secret.log. It is short and relative to the working
// directory of dtail and dserver, so the edited line does not wrap on the
// 80 column PTY; it returns it with the permitted file's absolute path,
// which the permission names, as reads are checked with resolved paths.
func writePermPathsDir(t *testing.T) (string, string) {
	t.Helper()
	dir := "permpaths.d"
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, name := range []string{"permitted.log", "secret.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("MARK line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readable, err := filepath.Abs(filepath.Join(dir, "permitted.log"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, readable
}
