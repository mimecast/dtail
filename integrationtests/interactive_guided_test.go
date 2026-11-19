package integrationtests

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The logs of the guided query tests: guidedSessionFile grows while an
// interactive dtail follows it, guidedTablesFile is written once.
const (
	guidedSessionFile = "interactive_guided.log.tmp"
	guidedTablesFile  = "interactive_tables.log.tmp"
)

// The lines of the guided query tests, of the default log format: STATS
// lines with a 15 character timestamp (so they fill $date) from stats.go,
// ORDERS lines with an 11 character one (no $date) from orders.go, whose
// prices are fixed per item, so max(price) is 3 for apple and 5 for pear
// however many lines a report covers, and RARE lines from rare.go.
const (
	guidedStatsLine = "INFO|20261118-120000|1|stats.go:56|8|13|7|0.21|1s|" +
		"MAPREDUCE:STATS|currentConnections=%d|lifetimeConnections=%d"
	guidedOrdersLine = "INFO|1118-120000|1|orders.go:10|8|13|7|0.21|1s|" +
		"MAPREDUCE:ORDERS|item=%s|price=%d"
	guidedRareLine = "INFO|1118-120000|1|rare.go:7|8|13|7|0.21|1s|" +
		"MAPREDUCE:RARE|rareField=%d"
)

// guidedCommaHint is the hint the listing after a finished select item
// shows (commaItemHint in internal/clients).
const guidedCommaHint = "(, adds another item)"

// sgrSequence matches an SGR (color) escape sequence.
var sgrSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// TestInteractiveGuidedQuerySession runs the target session of the guided
// query builder against dserver and serverless: an interactive dtail with
// --noColor follows the PLAIN lines of a growing log of two tables, and
// :query is typed with Tab alone choosing every word (from, listed with
// the tables in the same Tab, the table, select, the aggregation, its
// field, group by and its field), listing the tables, the select items and
// fields of the chosen table only, and after a finished item only the
// clauses that continue the query, with the comma hint. The applied query prints a correct result table and no followed
// line, :tail goes back to following lines, and :query switches to another
// query, all on one session. The output carries no SGR sequence.
func TestInteractiveGuidedQuerySession(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testInteractiveGuidedQuerySession(t, serverless)
		})
	}
}

func testInteractiveGuidedQuerySession(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestInteractiveGuidedQuerySession")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()
	if err := os.WriteFile(guidedSessionFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, guidedSessionFile)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout",
		"--logLevel", "debug", "--files", guidedSessionFile,
		"--grep", "PLAIN", "--noColor", "--interactive-query"}
	var serverLogs *processOutputCollector
	if !serverless {
		var port int
		port, serverLogs = startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port),
			"--trustAllHosts")
	}
	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go feedGuidedLines(feedCtx)

	output, err := runInteractivePTYCommand(ctx, argv, guidedSessionSteps())
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	checkGuidedSessionOutput(t, output)
	handlers := "Creating new server handler"
	if serverLogs != nil &&
		countSubstring(serverLogs.snapshot(), handlers) != 1 {
		t.Fatalf("expected one SSH session on the server, logs:\n%s",
			serverLogs.dump())
	}
}

// guidedSessionSteps are the PTY steps of the target session. Every Tab
// step waits for what the Tab before it drew: the completed line (the
// editor draws it again after the completion; it stays within the PTY's 80
// columns, so it is drawn as one piece) or a listing line. The query is run
// with Enter on the completed line; its result table is waited for by its
// header and dashes, which neither the typed line nor the echo contains.
// :show is sent a second after the :tail acknowledgement, so that followed
// lines arrive in between, as they did before typing started.
func guidedSessionSteps() []interactiveStep {
	tab := func(waitFor, input string) interactiveStep {
		return interactiveStep{WaitFor: waitFor, Input: input, Raw: true}
	}
	return []interactiveStep{
		// Typing starts once the probe answered and lines were followed
		// (the empty raw input sends nothing).
		tab(probeStoredMessage, ""),
		// The first Tab starts the query with from and lists the tables.
		tab("PLAIN line 3", ":query \t"),
		tab("table: ORDERS  STATS", "OR\t"),
		tab(":query from ORDERS ", "\t"),
		tab(":query from ORDERS select ", "\t"),
		tab("select item\r", "ma\t"),
		tab(":query from ORDERS select max(", "\t"),
		tab("field for max\r", "pr\t"),
		tab("select max(price)", " \t"),
		tab(guidedCommaHint, "gr\t"),
		tab("select max(price) group by ", "it\t"),
		tab("group by item ", "\t"),
		tab("next: where  order by  limit", "\r"),
		{WaitFor: guidedResultHeader, Input: ":tail"},
		{WaitFor: "following lines", Delay: time.Second, Input: ":show"},
		tab("workload=tail", ":query from ST\t"),
		tab(":query from STATS ", "select max(life\t"),
		tab("select max(lifetimeConnections)", "\r"),
		{WaitFor: " max(lifetimeConnections) \r\n---", Input: ":quit"},
	}
}

// checkGuidedSessionOutput checks what the target session printed beyond
// the steps' waits: the table scope of the listings, the result rows, no
// followed line while the query ran, followed lines after :tail, and no
// colors.
func checkGuidedSessionOutput(t *testing.T, output string) {
	t.Helper()
	items := outputSegment(output, "select item\r", ":query")
	checkContains(t, "select item listing", items, "item  price", "$caller")
	checkLacks(t, "ORDERS select item listing", items,
		"currentConnections", "$date", "STATS")
	maxField := outputSegment(output, "field for max\r", ":query")
	checkContains(t, "field for max listing", maxField, "item  price")
	checkLacks(t, "ORDERS field for max listing", maxField,
		"currentConnections", "$date", "count")
	next := outputSegment(output, "next: ", guidedCommaHint)
	checkLacks(t, "listing after the select item", next,
		"select", "from", "item", "price", "$", "count")

	table := outputSegment(output, guidedResultHeader, "query")
	checkContains(t, "result table", table, " 3.000000 ", " 5.000000 ")
	checkFollowSwitches(t, output)
	if found := sgrSequence.FindString(output); found != "" {
		t.Fatalf("--noColor output has the SGR sequence %q:\n%q", found, output)
	}
}

// The output the switch to the first query of the target session prints:
// the control loop's echo of the applied query (after "query applied") and
// the client's debug line of the committed update, the session's second
// generation (logged by every connection, "<server>|" before it).
const (
	guidedQueryEcho   = "query: from ORDERS select max(price) group by item"
	guidedQueryCommit = "Committed session spec|action|update|generation|2"
)

// guidedResultHeader starts the result table of the first query.
const guidedResultHeader = " max(price) \r\n---"

// checkFollowSwitches checks the switches between following lines and the
// query: from the switch to the query until its result table (seconds, at
// a PLAIN line every 100 ms) no followed line was printed, and after :tail
// followed lines were printed again.
//
// The quiet part starts after both the echo and the commit line. The
// control loop writes the echo straight to the terminal, while followed
// lines and the debug lines go through the logger, which prints its
// buffer up to 100 ms later: a followed line the client took before the
// update's acknowledgement can therefore be printed after the echo, but
// never after the commit line, which the logger took after it. Lines the
// typing pause held back are not printed after the switch: dserver (or the
// serverless read) drops them as output of the replaced generation, so
// only a line a writer already had in hand is printed once the :query
// line resumes the output, before the switch.
func checkFollowSwitches(t *testing.T, output string) {
	t.Helper()
	echo := strings.Index(output, guidedQueryEcho)
	commit := strings.Index(output, guidedQueryCommit)
	if echo < 0 || commit < 0 {
		t.Fatalf("no echo %q or commit line %q of the query:\n%s",
			guidedQueryEcho, guidedQueryCommit, output)
	}
	start := max(echo+len(guidedQueryEcho), commit+len(guidedQueryCommit))
	header := strings.Index(output[start:], guidedResultHeader)
	if header < 0 {
		t.Fatalf("no result table after the switch to the query:\n%s", output)
	}
	if quiet := output[start : start+header]; strings.Contains(quiet, "PLAIN line") {
		t.Fatalf("followed lines printed while the query ran:\n%s", quiet)
	}
	after := outputSegment(output, "following lines", "workload=tail")
	checkContains(t, "output after :tail", after, "PLAIN line")
}

// feedGuidedLines appends a STATS line, an apple and a pear ORDERS line and
// a PLAIN line to guidedSessionFile every 100 ms until ctx ends.
func feedGuidedLines(ctx context.Context) {
	for i := 0; ctx.Err() == nil; i++ {
		_ = appendLines(guidedSessionFile,
			fmt.Sprintf(guidedStatsLine, i%3, i),
			fmt.Sprintf(guidedOrdersLine, "apple", 3),
			fmt.Sprintf(guidedOrdersLine, "pear", 5),
			fmt.Sprintf("PLAIN line %d", i))
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestInteractiveQueryTableScope runs an interactive dtail against dserver
// and serverless on a log of three tables. STATS and ORDERS lines fill the
// probe's window; RARE lines come only before it, so the background probe
// does not find RARE. Tab after from lists STATS and ORDERS only; the
// fields, $variables and where values offered come only from the lines of
// the table typed after from ($date only for STATS, $caller values per
// table), whether the catalog got them from the background probe or from
// the table probe a Tab after from <table> asks for, which may answer
// before a later Tab; and once from RARE is typed, Tab has the table probe
// find it and completes its field, which :fields RARE lists too.
func TestInteractiveQueryTableScope(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testInteractiveQueryTableScope(t, serverless)
		})
	}
}

func testInteractiveQueryTableScope(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestInteractiveQueryTableScope")
	defer writeLogFileIgnoringError(testLogger)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()
	writeGuidedTablesLog(t)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout",
		"--logLevel", "debug", "--files", guidedTablesFile,
		"--grep", "MAPREDUCE", "--noColor", "--interactive-query"}
	if !serverless {
		port, _ := startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port),
			"--trustAllHosts")
	}
	output, err := runInteractivePTYCommand(ctx, argv, tableScopeSteps())
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	checkTableScopeOutput(t, output)
}

// tableScopeSteps are the PTY steps of TestInteractiveQueryTableScope. Each
// line is typed afresh (Ctrl-U clears the one before). A where value of
// $caller is the only one the table's lines have, so Tab inserts it. The
// first Tab after from RARE asks for the table probe and waits for it at
// most a second; the step waits for the client's debug line of its answer
// and Tabs again, which completes the field if the first Tab did not. If
// the first Tab already completed count(rareField), what the second Tab
// after the ')' does is not checked: the step waits only for the completed
// item, which stays on the line, and Ctrl-U clears the line after it.
func tableScopeSteps() []interactiveStep {
	tab := func(waitFor, input string) interactiveStep {
		return interactiveStep{WaitFor: waitFor, Input: input, Raw: true}
	}
	where := " select count($line) where $caller eq \t"
	return []interactiveStep{
		tab(probeStoredMessage, ":query from \t"),
		tab("table: ORDERS  STATS", "\x15:query from ORDERS select \t"),
		tab("select item\r", "\x15:query from STATS select \t"),
		tab("$date", "\x15:query from ORDERS"+where),
		tab(`eq "orders.go:10" `, "\x15:query from STATS"+where),
		tab(`eq "stats.go:56" `, "\x15:query from RARE select count(ra\t"),
		tab("|RARE|formats|", "\t"),
		tab("count(rareField)", "\x15:fields RARE\r"),
		{WaitFor: "table RARE probed on its own", Input: ":quit"},
	}
}

// checkTableScopeOutput checks the listings of TestInteractiveQueryTableScope:
// the tables found by the background probe, and the select listings (from
// the background or the table probe) and :fields RARE scoped to their table.
func checkTableScopeOutput(t *testing.T, output string) {
	t.Helper()
	tables := outputSegment(output, "table: ", "\r\n")
	checkLacks(t, "table listing", tables, "RARE")
	orders := outputSegment(output, "select item\r", ":query")
	checkContains(t, "ORDERS select listing", orders, "item  price", "$caller")
	checkLacks(t, "ORDERS select listing", orders,
		"currentConnections", "$date")
	statsStart := strings.Index(output, "from STATS select ")
	if statsStart < 0 {
		t.Fatalf("no STATS select line:\n%s", output)
	}
	stats := outputSegment(output[statsStart:], "select item\r", ":query")
	checkContains(t, "STATS select listing", stats,
		"currentConnections  lifetimeConnections", "$date")
	checkLacks(t, "STATS select listing", stats, "price")
	rare := outputSegment(output, "table RARE:", "probed on its own")
	checkContains(t, ":fields RARE", rare, "rareField", "rare.go:7")
	checkLacks(t, ":fields RARE", rare, "stats.go", "orders.go",
		"currentConnections")
}

// writeGuidedTablesLog writes guidedTablesFile: two STATS lines, three
// RARE lines and then 4,000 lines (about 450 KiB) of STATS and ORDERS in
// turn, so that the probe's window (the last 256 KiB) holds no RARE line;
// the first line, which the probe also reads, is a STATS one.
func writeGuidedTablesLog(t *testing.T) {
	t.Helper()
	var lines strings.Builder
	for i := range 2 {
		fmt.Fprintf(&lines, guidedStatsLine+"\n", i, i)
	}
	for i := range 3 {
		fmt.Fprintf(&lines, guidedRareLine+"\n", i)
	}
	for i := range 2000 {
		fmt.Fprintf(&lines, guidedStatsLine+"\n", i%3, i)
		fmt.Fprintf(&lines, guidedOrdersLine+"\n", "apple", 3)
	}
	err := os.WriteFile(guidedTablesFile, []byte(lines.String()), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, guidedTablesFile)
}

// outputSegment returns the part of output from the first start up to the
// first end after it (the rest of output without one), or "" if output has
// no start.
func outputSegment(output, start, end string) string {
	i := strings.Index(output, start)
	if i < 0 {
		return ""
	}
	segment := output[i+len(start):]
	if j := strings.Index(segment, end); j >= 0 {
		segment = segment[:j]
	}
	return segment
}

// checkContains fails the test unless segment, named what, holds every
// needle.
func checkContains(t *testing.T, what, segment string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(segment, needle) {
			t.Fatalf("%s lacks %q:\n%q", what, needle, segment)
		}
	}
}

// checkLacks fails the test if segment, named what, holds a needle, or is
// empty (not found in the output), which no needle could be found in.
func checkLacks(t *testing.T, what, segment string, needles ...string) {
	t.Helper()
	if strings.TrimSpace(segment) == "" {
		t.Fatalf("%s not found in the output", what)
	}
	for _, needle := range needles {
		if strings.Contains(segment, needle) {
			t.Fatalf("%s has %q:\n%q", what, needle, segment)
		}
	}
}
