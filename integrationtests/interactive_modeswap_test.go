package integrationtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// modeSwapFile is the followed file of TestDTailQueryTailCommands.
const modeSwapFile = "interactive_modeswap.log.tmp"

// TestDTailQueryTailCommands runs an interactive dtail on a PTY against
// dserver and serverless, following the lines of a growing file, and
// switches its session with :query and :tail from following lines to a
// query, back, and to another query: each switch is acknowledged, the
// query's result table and the followed lines show up in turn, the session
// is the same throughout (one server handler, generations 2 to 4 committed
// by updates) and dtail exits 0 on :quit.
func TestDTailQueryTailCommands(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testDTailQueryTailCommands(t, serverless)
		})
	}
}

func testDTailQueryTailCommands(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestDTailQueryTailCommands")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()
	if err := os.WriteFile(modeSwapFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, modeSwapFile)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "debug",
		"--files", modeSwapFile, "--grep", "PLAIN", "--plain", "--interactive-query"}
	var serverLogs *processOutputCollector
	if !serverless {
		var port int
		port, serverLogs = startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port), "--trustAllHosts")
	}
	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go feedModeSwapLines(feedCtx)

	output, err := runInteractivePTYCommand(ctx, argv, modeSwapSteps())
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	for _, answer := range []string{"query applied", "following lines"} {
		if !strings.Contains(output, answer) {
			t.Fatalf("no %q answer:\n%s", answer, output)
		}
	}
	for generation := 2; generation <= 4; generation++ {
		if !strings.Contains(output, fmt.Sprintf("Committed session spec|action|update|generation|%d", generation)) {
			t.Fatalf("no update committed at generation %d:\n%s", generation, output)
		}
	}
	if serverLogs != nil && countSubstring(serverLogs.snapshot(), "Creating new server handler") != 1 {
		t.Fatalf("expected one SSH session on the server, logs:\n%s", serverLogs.dump())
	}
}

// modeSwapSteps are the PTY steps: each switch waits for what the one
// before it shows (followed lines, a result table, the acknowledgement).
// The result tables are waited for by their header and dashes, which
// neither the typed line nor the acknowledgement contains.
func modeSwapSteps() []interactiveStep {
	return []interactiveStep{
		{WaitFor: "PLAIN line", Input: ":query from STATS select count($line) group by $hostname interval 1"},
		{WaitFor: "count($line) \r\n---", Input: ":show"},
		{WaitFor: "workload=query", Input: ":tail WARN"},
		// Only the lines followed after :tail WARN match "WARN line" (an
		// empty Input would send an empty line, which pauses the output).
		{WaitFor: "WARN line", Input: ":show"},
		{WaitFor: "workload=tail", Input: ":query from STATS select max(currentConnections) group by $hostname interval 1"},
		{WaitFor: "max(currentConnections) \r\n---", Input: ":quit"},
	}
}

// feedModeSwapLines appends a PLAIN, a WARN and a STATS line to the
// followed file every 100 ms until ctx ends.
func feedModeSwapLines(ctx context.Context) {
	for i := 0; ctx.Err() == nil; i++ {
		_ = appendLines(modeSwapFile, fmt.Sprintf("PLAIN line %d", i), fmt.Sprintf("WARN line %d", i),
			fmt.Sprintf("INFO|1118-120000|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
				"currentConnections=%d|lifetimeConnections=%d", i%3, i))
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
