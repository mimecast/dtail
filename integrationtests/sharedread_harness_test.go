package integrationtests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file holds the harness of the shared read tests (sharedread_test.go):
// every scenario runs once with shared reads on (the default) and once with
// "SharedReadsDisable": true, and the two runs must produce byte-identical
// output. Synchronisation polls files and server logs with timeouts; nothing
// waits a fixed time for another process.

const (
	// sharedReadTimeout bounds every wait for another process. It is generous
	// so that a loaded machine slows the tests down instead of failing them.
	sharedReadTimeout = 90 * time.Second
	// sharedReadPoll is the polling interval of all waits.
	sharedReadPoll = 50 * time.Millisecond
	// sharedReadSyncMarker starts the lines syncFollowClients writes. Every
	// client regex of the shared read tests matches it.
	sharedReadSyncMarker = "SYNC "
	// sharedReadFillerLines follow the sync lines, so that no before context
	// of a later match reaches back to a sync line: the number of sync lines a
	// client sees differs from run to run.
	sharedReadFillerLines = 5
	// sharedReadOutputTail is how much of a client's output file a wait for a
	// marker line reads: markers are written last.
	sharedReadOutputTail = 64 * 1024
	// sharedReadPrefillMarker starts the lines createPrefilledFile writes
	// before a test's clients start; sharedReadPrefillLines is their number.
	sharedReadPrefillMarker = "PREFILL"
	sharedReadPrefillLines  = 1000
)

// sharedReadMode is one of the two server configurations every shared read
// test runs with.
type sharedReadMode struct {
	name    string
	disable bool
}

// shared reports whether dserver shares reads in this mode.
func (m sharedReadMode) shared() bool {
	return !m.disable
}

// sharedReadServer is a dserver started for one mode of a shared read test.
type sharedReadServer struct {
	address string
	logs    *safeLineLog
}

// followClient is a dtail client whose stdout and stderr go to outFile.
type followClient struct {
	name    string
	outFile string
	cmd     *exec.Cmd
}

// sharedReadModes returns the modes in the order the tests run them.
func sharedReadModes() []sharedReadMode {
	return []sharedReadMode{
		{name: "SharingOn"},
		{name: "SharingOff", disable: true},
	}
}

// runSharedReadModes runs scenario once per mode, each in a subtest, and
// requires that every output the scenario returns is byte-identical in both
// modes.
func runSharedReadModes(t *testing.T, scenario func(*testing.T, sharedReadMode) map[string]string) {
	t.Helper()
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)

	results := make(map[string]map[string]string)
	for _, mode := range sharedReadModes() {
		if !t.Run(mode.name, func(t *testing.T) {
			results[mode.name] = scenario(t, mode)
		}) {
			return
		}
	}

	on, off := results["SharingOn"], results["SharingOff"]
	if len(on) != len(off) {
		t.Fatalf("sharing on returned %d outputs, sharing off %d", len(on), len(off))
	}
	for name, want := range off {
		got, ok := on[name]
		if !ok {
			t.Errorf("output %s missing with sharing on", name)
			continue
		}
		if got != want {
			t.Errorf("output %s differs between sharing on and off:\n%s", name, firstDifference(got, want))
		}
	}
}

// startSharedReadServer writes a config with the given Server section plus
// the mode's SharedReadsDisable and starts dserver with it on 127.0.0.1. The
// server is killed when the test ends.
func startSharedReadServer(t *testing.T, mode sharedReadMode, server map[string]any) *sharedReadServer {
	t.Helper()

	section := map[string]any{"SharedReadsDisable": mode.disable}
	for key, value := range server {
		section[key] = value
	}
	content, err := json.MarshalIndent(map[string]any{"Server": section}, "", "  ")
	if err != nil {
		t.Fatalf("marshal dserver config: %v", err)
	}
	cfgFile := filepath.Join(t.TempDir(), "dtail.json")
	if err := os.WriteFile(cfgFile, content, 0o600); err != nil {
		t.Fatalf("write dserver config: %v", err)
	}

	const bindAddress = "127.0.0.1"
	port := getUniquePortNumber()
	ctx, cancel := context.WithCancel(context.Background())
	logs := &safeLineLog{}
	cmd := exec.CommandContext(ctx, "../dserver",
		"--cfg", cfgFile,
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", bindAddress,
		"--port", strconv.Itoa(port),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("open dserver stdout: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start dserver: %v", err)
	}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanToLog(stdout, logs)
	}()
	t.Cleanup(func() {
		cancel()
		<-scanned
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("dserver log (%s):\n%s", mode.name, logs.String())
		}
	})

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Fatalf("wait for dserver: %v", err)
	}
	return &sharedReadServer{address: fmt.Sprintf("%s:%d", bindAddress, port), logs: logs}
}

// count returns how many log lines contain substr.
func (l *safeLineLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// waitForLog waits until at least n server log lines contain substr.
func (s *sharedReadServer) waitForLog(t *testing.T, substr string, n int) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("%d server log lines containing %q", n, substr), func() bool {
		return s.logs.count(substr) >= n
	})
}

// requireLogCount fails the test unless exactly n log lines contain substr.
func (s *sharedReadServer) requireLogCount(t *testing.T, substr string, n int) {
	t.Helper()
	if got := s.logs.count(substr); got != n {
		t.Errorf("server log has %d lines containing %q, want %d", got, substr, n)
	}
}

// waitForReaders waits until n sessions started reading file. The log line
// is written before a private reader opened the file, so the caller still
// has to synchronise with the readers (syncFollowClients) before it writes
// lines the readers must see.
func (s *sharedReadServer) waitForReaders(t *testing.T, file string, n int) {
	t.Helper()
	s.waitForLog(t, "|Start reading|"+file+"|", n)
}

// startFollowClient starts dtail following file on the server; extra holds
// the client specific flags such as --grep.
func startFollowClient(t *testing.T, server *sharedReadServer, name, file string, extra ...string) *followClient {
	t.Helper()

	cfgFile := writeFollowClientConfig(t)
	outFile := "sharedread_" + name + ".out.tmp"
	out, err := os.Create(outFile)
	if err != nil {
		t.Fatalf("create client output %s: %v", outFile, err)
	}
	defer closeIgnoringError(out)

	args := append([]string{
		"--cfg", cfgFile,
		"--logger", "stdout",
		"--logLevel", "error",
		"--servers", server.address,
		"--files", file,
		"--trustAllHosts",
		"--noColor",
	}, extra...)
	cmd := exec.Command("../dtail", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dtail %s: %v", name, err)
	}
	t.Cleanup(func() {
		// Kill also ends a SIGSTOPped client.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if !t.Failed() {
			removeIgnoringError(outFile)
		}
	})
	return &followClient{name: name, outFile: outFile, cmd: cmd}
}

// writeFollowClientConfig writes a client config whose known hosts file is
// the client's own, in a temporary directory, and returns its path. With
// --trustAllHosts a client adds the server's key to its known hosts file
// through a temporary file of fixed name in the same directory, so clients
// started together must not share one.
func writeFollowClientConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		t.Fatalf("create client known hosts file: %v", err)
	}
	content, err := json.Marshal(map[string]any{"Client": map[string]any{"KnownHostsPath": knownHosts}})
	if err != nil {
		t.Fatalf("marshal client config: %v", err)
	}
	cfgFile := filepath.Join(dir, "client.json")
	if err := os.WriteFile(cfgFile, content, 0o600); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	return cfgFile
}

// signal sends sig to the client process.
func (c *followClient) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %v to dtail %s: %v", sig, c.name, err)
	}
}

// waitForOutput waits until the end of the client's output contains substr.
func (c *followClient) waitForOutput(t *testing.T, substr string) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("dtail %s printing %q", c.name, substr), func() bool {
		return strings.Contains(c.outputTail(t), substr)
	})
}

// outputTail returns the last sharedReadOutputTail bytes of the output.
func (c *followClient) outputTail(t *testing.T) string {
	t.Helper()
	fd, err := os.Open(c.outFile)
	if err != nil {
		t.Fatalf("open client output: %v", err)
	}
	defer closeIgnoringError(fd)
	info, err := fd.Stat()
	if err != nil {
		t.Fatalf("stat client output: %v", err)
	}
	offset := max(info.Size()-sharedReadOutputTail, 0)
	data, err := io.ReadAll(io.NewSectionReader(fd, offset, info.Size()-offset))
	if err != nil {
		t.Fatalf("read client output: %v", err)
	}
	return string(data)
}

// output returns the whole client output.
func (c *followClient) output(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(c.outFile)
	if err != nil {
		t.Fatalf("read client output: %v", err)
	}
	return string(data)
}

// outputAfterSync returns the client output after its last sync line. Line
// numbers of REMOTE lines are made relative to the sync line's, because they
// count from the session's join and the number of sync lines varies.
func (c *followClient) outputAfterSync(t *testing.T) string {
	t.Helper()
	lines := strings.SplitAfter(c.output(t), "\n")
	last := -1
	for i, line := range lines {
		if strings.Contains(line, sharedReadSyncMarker) {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("dtail %s printed no sync line", c.name)
	}
	base, _ := remoteLineNumber(lines[last])
	var sb strings.Builder
	for _, line := range lines[last+1:] {
		number, ok := remoteLineNumber(line)
		if !ok {
			sb.WriteString(line)
			continue
		}
		fields := strings.SplitN(line, "|", 5)
		fields[3] = strconv.FormatUint(number-base, 10)
		sb.WriteString(strings.Join(fields, "|"))
	}
	return sb.String()
}

// remoteLineNumber returns the line number of a REMOTE output line:
// REMOTE|host|percent|number|file|line.
func remoteLineNumber(line string) (uint64, bool) {
	fields := strings.SplitN(line, "|", 5)
	if len(fields) < 5 || fields[0] != "REMOTE" {
		return 0, false
	}
	number, err := strconv.ParseUint(fields[3], 10, 64)
	return number, err == nil
}

// syncFollowClients appends sync lines to file until every client printed
// one: from then on each client's session reads every line appended later.
// It then appends the filler lines (see sharedReadFillerLines) and returns
// them, as they belong to what the clients print after the sync.
func syncFollowClients(t *testing.T, file string, clients ...*followClient) []string {
	t.Helper()
	var n int
	pollUntil(t, "clients printing a sync line", func() bool {
		n++
		appendFileLines(t, file, fmt.Sprintf("%s%d", sharedReadSyncMarker, n))
		for _, client := range clients {
			if !strings.Contains(client.outputTail(t), sharedReadSyncMarker) {
				return false
			}
		}
		return true
	})
	filler := make([]string, sharedReadFillerLines)
	for i := range filler {
		filler[i] = fmt.Sprintf("filler %d", i)
	}
	appendFileLines(t, file, filler...)
	return filler
}

// appendFileLines appends lines to file in a single write.
func appendFileLines(t *testing.T, file string, lines ...string) {
	t.Helper()
	appendToFile(t, file, strings.Join(lines, "\n")+"\n")
}

// appendToFile appends data to file in a single write.
func appendToFile(t *testing.T, file, data string) {
	t.Helper()
	fd, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open %s for appending: %v", file, err)
	}
	defer closeIgnoringError(fd)
	if _, err := fd.WriteString(data); err != nil {
		t.Fatalf("append to %s: %v", file, err)
	}
}

// createEmptyFile creates file, or truncates it, and removes it when the
// test ends.
func createEmptyFile(t *testing.T, file string) {
	t.Helper()
	createFileWithLines(t, file, nil)
}

// createFileWithLines creates file holding lines, or replaces it, and removes
// it when the test ends.
func createFileWithLines(t *testing.T, file string, lines []string) {
	t.Helper()
	var data []byte
	if len(lines) > 0 {
		data = []byte(strings.Join(lines, "\n") + "\n")
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatalf("create %s: %v", file, err)
	}
	cleanupFiles(t, file)
}

// createPrefilledFile creates file holding sharedReadPrefillLines lines that
// match every client regex of the shared read tests. A follow read starts at
// the end of the file, so no client may print one of them.
func createPrefilledFile(t *testing.T, file string) {
	t.Helper()
	createFileWithLines(t, file, markedLines(sharedReadPrefillMarker, sharedReadPrefillLines))
}

// markedLines returns n lines starting with marker that match every client
// regex of the shared read tests.
func markedLines(marker string, n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("%s %04d 7 foo bar", marker, i)
	}
	return lines
}

// requireNoMarkedLines fails the test when the client's whole output holds
// a line containing marker: a line that was in the file before the client's
// read started.
func (c *followClient) requireNoMarkedLines(t *testing.T, marker string) {
	t.Helper()
	if n := strings.Count(c.output(t), marker); n > 0 {
		t.Errorf("dtail %s printed %d lines containing %q, which were in the file before its read started",
			c.name, n, marker)
	}
}

// outputWithoutSync returns the whole output of a --plain client without
// its sync lines, whose number varies from run to run.
func (c *followClient) outputWithoutSync(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.SplitAfter(c.output(t), "\n") {
		if !strings.Contains(line, sharedReadSyncMarker) {
			sb.WriteString(line)
		}
	}
	return sb.String()
}

// matchingLines returns the lines matching expr, each with its newline: what
// a --plain client without context prints for them.
func matchingLines(t *testing.T, expr string, lines []string) string {
	t.Helper()
	re := regexp.MustCompile(expr)
	var sb strings.Builder
	for _, line := range lines {
		if re.MatchString(line) {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// pollUntil polls cond until it holds and fails the test after
// sharedReadTimeout.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(sharedReadTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", sharedReadTimeout, what)
		}
		time.Sleep(sharedReadPoll)
	}
}

// firstDifference describes where two outputs start to differ.
func firstDifference(got, want string) string {
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < min(len(gotLines), len(wantLines)); i++ {
		if gotLines[i] != wantLines[i] {
			return fmt.Sprintf("line %d: got %q, want %q (got %d lines, want %d)",
				i+1, gotLines[i], wantLines[i], len(gotLines), len(wantLines))
		}
	}
	return fmt.Sprintf("got %d lines, want %d", len(gotLines), len(wantLines))
}
