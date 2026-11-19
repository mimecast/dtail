package integrationtests

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// probeStoredMessage is the debug line the client logs when a probe answer
// went into its field catalog (interactiveProber.probe).
const probeStoredMessage = "Probe answer stored in the field catalog"

// TestInteractiveProbeFillsCatalog runs real interactive sessions on a PTY
// and requires the background prober's answer in the client's field
// catalog, logged at debug level once stored: against dserver with the
// client handler (dtail with a grep) and with the mapreduce handler (dtail
// with a query), and serverless. The answer must come from the one sampled
// file and have found fields (not formats 0), and no probe may fail.
func TestInteractiveProbeFillsCatalog(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, tt := range []struct {
		name       string
		serverless bool
		args       []string
	}{
		{name: "grep", args: []string{"--grep", "MAPREDUCE"}},
		{name: "query", args: []string{"--query", "from STATS select count($line) group by $hostname"}},
		{name: "serverless", serverless: true, args: []string{"--grep", "MAPREDUCE"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testInteractiveProbeFillsCatalog(t, tt.serverless, tt.args)
		})
	}
}

func testInteractiveProbeFillsCatalog(t *testing.T, serverless bool, modeArgs []string) {
	testLogger := NewTestLogger("TestInteractiveProbeFillsCatalog")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	logFile := "interactive_probe.log.tmp"
	var lines strings.Builder
	for i := range 50 {
		fmt.Fprintf(&lines, "INFO|1118-120000|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
			"currentConnections=%d|lifetimeConnections=%d\n", i%3, i)
	}
	if err := os.WriteFile(logFile, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, logFile)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "debug",
		"--files", logFile, "--plain", "--interactive-query"}
	var serverLogs *processOutputCollector
	if !serverless {
		var port int
		port, serverLogs = startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port), "--trustAllHosts")
	}
	argv = append(argv, modeArgs...)
	steps := []interactiveStep{{WaitFor: probeStoredMessage, Input: ":quit"}}
	if slices.Contains(modeArgs, "--query") {
		// Tab inside the --query value completes a field the probe found.
		steps = []interactiveStep{
			{WaitFor: probeStoredMessage, Input: ":reload --query 'from STATS select count(curr\t", Raw: true},
			{WaitFor: "count(currentConnections)", Input: "\x15:quit"},
		}
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	stored := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, probeStoredMessage) {
			stored = line
			break
		}
	}
	if stored == "" || strings.Contains(stored, "formats|0") || !strings.Contains(stored, "filesSampled|1") {
		t.Fatalf("no probe answer with fields stored (line %q), output:\n%s", stored, output)
	}
	t.Logf("stored: %s", stored)
	if strings.Contains(output, "Probe failed") {
		t.Fatalf("probe failed, output:\n%s", output)
	}
	// dserver's log is flushed asynchronously, so its line may come after
	// the client's.
	if serverLogs != nil {
		if err := waitForCollectorSubstring(ctx, serverLogs, probeAnsweredMessage); err != nil {
			t.Fatalf("dserver logged no probe: %v, logs:\n%s", err, serverLogs.dump())
		}
	}
}

// probeAnsweredMessage is the line dserver logs for every probe it answered.
const probeAnsweredMessage = "Probe answered"

// TestNonInteractiveClientSendsNoProbe: only interactive sessions probe; a
// plain dcat reads its file and dserver answers no probe.
func TestNonInteractiveClientSendsNoProbe(t *testing.T) {
	skipIfNotIntegrationTest(t)
	testLogger := NewTestLogger("TestNonInteractiveClientSendsNoProbe")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	logFile := "noninteractive_probe.log.tmp"
	if err := os.WriteFile(logFile, []byte("plain line one\nplain line two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, logFile)
	port, serverLogs := startInteractiveTestServer(ctx, t)
	output, err := exec.CommandContext(ctx, "../dcat", "--cfg", "none", "--logger", "stdout",
		"--logLevel", "info", "--plain", "--trustAllHosts",
		"--servers", fmt.Sprintf("localhost:%d", port), "--files", logFile).CombinedOutput()
	if err != nil {
		t.Fatalf("dcat: %v\noutput:\n%s", err, output)
	}
	if !strings.Contains(string(output), "plain line two") {
		t.Fatalf("dcat output lacks the file:\n%s", output)
	}
	// dserver logs its goodbye after every command of the session, so once
	// it is in, a probe's line would be too.
	if err := waitForCollectorSubstring(ctx, serverLogs, "Good bye Mister!"); err != nil {
		t.Fatalf("no session in the server log: %v\n%s", err, serverLogs.dump())
	}
	if countSubstring(serverLogs.snapshot(), probeAnsweredMessage) != 0 {
		t.Fatalf("dserver answered a probe of a plain dcat, logs:\n%s", serverLogs.dump())
	}
}

// TestInteractiveFieldsAndProbe runs :fields and :probe in real interactive
// dtail query sessions on a PTY, against dserver and serverless: once the
// background probe stored its answer, ":fields stats" lists the STATS table
// with its fields and keeps the output paused; :probe then resumes it,
// probes again and reports the answering server; ":fields nope" names the
// known tables.
func TestInteractiveFieldsAndProbe(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, serverless := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverless=%v", serverless), func(t *testing.T) {
			testInteractiveFieldsAndProbe(t, serverless)
		})
	}
}

func testInteractiveFieldsAndProbe(t *testing.T, serverless bool) {
	testLogger := NewTestLogger("TestInteractiveFieldsAndProbe")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	logFile := "interactive_fields.log.tmp"
	var lines strings.Builder
	for i := range 20 {
		fmt.Fprintf(&lines, "INFO|1118-120000|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
			"currentConnections=%d|lifetimeConnections=%d\n", i%3, i)
	}
	if err := os.WriteFile(logFile, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, logFile)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "debug",
		"--files", logFile, "--plain", "--interactive-query",
		"--query", "from STATS select count($line) group by $hostname"}
	if !serverless {
		port, _ := startInteractiveTestServer(ctx, t)
		argv = append(argv, "--servers", fmt.Sprintf("localhost:%d", port), "--trustAllHosts")
	}
	steps := []interactiveStep{
		{WaitFor: probeStoredMessage, Input: ":fields stats"},
		{WaitFor: "table STATS:", Input: ":probe"},
		{WaitFor: "probe answered by", Input: ":fields nope"},
		{WaitFor: "no table nope found; tables: STATS", Input: ":quit"},
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
	for _, want := range []string{"fields found by the probe of ", "currentConnections", "lifetimeConnections",
		"output paused, press Enter to resume", "output resumed", "; :fields shows what was found"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Probe failed") {
		t.Fatalf("probe failed, output:\n%s", output)
	}
}

// TestInteractiveProbeUnreachableFirstServer: with two servers that refuse
// connections next to a healthy one in --servers, :probe reports the
// healthy server's answer within its 5 s wait, for dtail (which retries the
// unreachable servers) and for dgrep --interactive-query (which does not).
// A connection whose start ended without a session has its handler shut
// down, so the probe rounds no longer wait their 30 s timeout for it
// (docs/dev/probe.md, "Which connections"). The client shuffles the
// servers; with every server a probe target the outcome does not depend on
// their order (a refusing one is often among the first); the unit tests
// TestProbeUnreachableFirstServer and TestProbeTargetsPreferSessionsUp fix
// the order.
func TestInteractiveProbeUnreachableFirstServer(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, client := range []string{"dtail", "dgrep"} {
		t.Run(client, func(t *testing.T) {
			testInteractiveProbeUnreachableFirstServer(t, client)
		})
	}
}

func testInteractiveProbeUnreachableFirstServer(t *testing.T, client string) {
	testLogger := NewTestLogger("TestInteractiveProbeUnreachableFirstServer")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	logFile := "interactive_probe_unreachable.log.tmp"
	line := "INFO|1118-120000|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=1\n"
	if err := os.WriteFile(logFile, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, logFile)

	port, _ := startInteractiveTestServer(ctx, t)
	healthy := fmt.Sprintf("localhost:%d", port)
	servers := closedLocalPortAddress(t) + "," + closedLocalPortAddress(t) + "," + healthy
	argv := []string{"../" + client, "--cfg", "none", "--logger", "stdout", "--logLevel", "debug",
		"--servers", servers, "--files", logFile, "--grep", "MAPREDUCE", "--plain",
		"--trustAllHosts", "--interactive-query"}
	output, err := runInteractivePTYCommand(ctx, argv, []interactiveStep{
		{WaitFor: probeStoredMessage, Input: ":probe"},
		{WaitFor: "probe answered by " + healthy, Input: ":quit"},
	})
	var exitErr *exec.ExitError
	// Both exit 1 for the servers they could not connect to.
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("run interactive %s: %v\noutput:\n%s", client, err, output)
	}
	if strings.Contains(output, "no answer within") {
		t.Fatalf(":probe timed out, output:\n%s", output)
	}
}

// closedLocalPortAddress returns host:port of a local TCP port nothing
// listens on (a listener's port, closed again): a dial is refused at once.
func closedLocalPortAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return address
}
