package integrationtests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type interactiveStep struct {
	// At is a fixed delay (from the writer start) before Input is sent. It is
	// only consulted when WaitFor and WaitForFile are empty.
	At time.Duration
	// WaitFor, when set, makes the step block until this substring appears in
	// the accumulated PTY output before Input is sent (bounded by a timeout).
	// This replaces racy fixed-offset timing with deterministic
	// synchronization on observed output.
	WaitFor string
	// WaitForFile, when set, makes the step block (after WaitFor) until this
	// file exists before Input is sent (bounded by the same timeout). The test
	// creates the file once an event outside the PTY output happened, e.g. a
	// server log line, so a step can also wait for the server.
	WaitForFile string
	Input       string
}

// processOutputCollector collects the output lines of a process, see
// startInteractiveTestServer.
type processOutputCollector struct {
	mu    sync.Mutex
	lines []string
	// partial holds the bytes of an output line whose newline did not
	// arrive yet.
	partial []byte
}

func TestDTailInteractiveReloadReusesSessionAndDropsLateOldMatches(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDTailInteractiveReloadReusesSessionAndDropsLateOldMatches")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	followFile := "interactive_dtail_reload.tmp"
	if err := os.WriteFile(followFile, nil, 0600); err != nil {
		t.Fatalf("unable to create follow file: %v", err)
	}
	cleanupFiles(t, followFile, "interactive_dtail_reload.stdout.tmp")

	port, serverLogs := startInteractiveTestServer(ctx, t)

	writerDone := make(chan error, 1)
	go func() {
		if err := waitForCollectorSubstring(ctx, serverLogs, "Start reading|"+followFile+"|"+followFile); err != nil {
			writerDone <- err
			return
		}
		writerDone <- appendLinesOnSchedule(ctx, followFile, []interactiveStep{
			{At: 100 * time.Millisecond, Input: "ERROR initial"},
			{At: 3000 * time.Millisecond, Input: "ERROR late"},
			{At: 3200 * time.Millisecond, Input: "WARN live"},
		})
	}()

	clientOutput, err := runInteractivePTYCommand(ctx, []string{
		"../dtail",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port),
		"--files", followFile,
		"--grep", "ERROR",
		"--plain",
		"--trustAllHosts",
		"--interactive-query",
	}, []interactiveStep{
		{At: 3 * time.Second, Input: ":reload --grep WARN"},
		{At: 6 * time.Second, Input: ":quit"},
	})
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, clientOutput)
	}

	if err := <-writerDone; err != nil {
		t.Fatalf("write follow file: %v", err)
	}

	if !strings.Contains(clientOutput, "ERROR initial") {
		t.Fatalf("expected initial ERROR line in output:\n%s", clientOutput)
	}
	if !strings.Contains(clientOutput, "WARN live") {
		t.Fatalf("expected WARN line after reload in output:\n%s", clientOutput)
	}
	if strings.Contains(clientOutput, "ERROR late") {
		t.Fatalf("unexpected stale ERROR line after reload:\n%s", clientOutput)
	}
	if !strings.Contains(clientOutput, "reload applied successfully") {
		t.Fatalf("expected reload success message in output:\n%s", clientOutput)
	}
	if countSubstring(serverLogs.snapshot(), "Creating new server handler") != 1 {
		t.Fatalf("expected one SSH session on the server, logs:\n%s", serverLogs.dump())
	}
}

func TestDTailInteractiveReloadReusesSessionOnImmediateBoundaryAndDropsLateOldMatches(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDTailInteractiveReloadReusesSessionOnImmediateBoundaryAndDropsLateOldMatches")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	followFile := "interactive_dtail_reload_immediate.tmp"
	// Start with an empty follow file; the server tails appended lines (it does
	// not replay pre-existing content), so all matches are fed as appends below.
	if err := os.WriteFile(followFile, nil, 0600); err != nil {
		t.Fatalf("unable to create follow file: %v", err)
	}
	cleanupFiles(t, followFile, "interactive_dtail_reload_immediate.stdout.tmp")

	port, serverLogs := startInteractiveTestServer(ctx, t)

	// Feed follow-file content at deterministic points instead of on fixed
	// offsets from an independent clock (the previous racy design, which under
	// load could append the boundary line after the reload had already switched
	// the grep to WARN, so it never matched -> "expected first-generation ERROR
	// line before reload").
	//
	// Sequence, each step gated on an observed barrier:
	//  1. wait for the first "Start reading" (session tailing), then append the
	//     first-generation "ERROR boundary" match. The reload step blocks until
	//     this line is seen in the client output, so it always precedes reload.
	//  2. wait for the second "Start reading" (the reload dispatches a fresh
	//     read command under the new grep-WARN generation; that log is emitted
	//     as the new read begins, a sound barrier), then append the post-reload
	//     lines. "ERROR late" must be dropped (no longer matches WARN) and
	//     "WARN live" must appear.
	readNeedle := "Start reading|" + followFile + "|" + followFile
	writerDone := make(chan error, 1)
	go func() {
		if err := waitForCollectorSubstring(ctx, serverLogs, readNeedle); err != nil {
			writerDone <- err
			return
		}
		if err := appendLines(followFile, "ERROR boundary"); err != nil {
			writerDone <- err
			return
		}
		if err := waitForCollectorCount(ctx, serverLogs, readNeedle, 2); err != nil {
			writerDone <- err
			return
		}
		writerDone <- appendLines(followFile, "ERROR late", "WARN live")
	}()

	clientOutput, err := runInteractivePTYCommand(ctx, []string{
		"../dtail",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port),
		"--files", followFile,
		"--grep", "ERROR",
		"--plain",
		"--trustAllHosts",
		"--interactive-query",
	}, []interactiveStep{
		// Reload only after the first-generation ERROR match has actually been
		// emitted, so the boundary line deterministically precedes the reload
		// regardless of connection-setup latency under load.
		{WaitFor: "ERROR boundary", Input: ":reload --grep WARN"},
		// Quit only after the new-generation WARN match has been emitted, so the
		// captured output deterministically contains it before the session ends.
		{WaitFor: "WARN live", Input: ":quit"},
	})
	if err != nil {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, clientOutput)
	}

	if err := <-writerDone; err != nil {
		t.Fatalf("write follow file: %v", err)
	}

	if !strings.Contains(clientOutput, "WARN live") {
		t.Fatalf("expected WARN line after reload in output:\n%s", clientOutput)
	}
	if !strings.Contains(clientOutput, "ERROR boundary") {
		t.Fatalf("expected first-generation ERROR line before reload in output:\n%s", clientOutput)
	}
	if strings.Contains(clientOutput, "ERROR late") {
		t.Fatalf("unexpected stale ERROR line after reload:\n%s", clientOutput)
	}
	if !strings.Contains(clientOutput, "reload applied successfully") {
		t.Fatalf("expected reload success message in output:\n%s", clientOutput)
	}
	if boundaryIdx := strings.Index(clientOutput, "ERROR boundary"); boundaryIdx == -1 || boundaryIdx > strings.Index(clientOutput, "reload applied successfully") {
		t.Fatalf("expected first-generation ERROR output to precede reload success:\n%s", clientOutput)
	}
	if countSubstring(serverLogs.snapshot(), "Creating new server handler") != 1 {
		t.Fatalf("expected one SSH session on the server, logs:\n%s", serverLogs.dump())
	}
}

func TestDGrepInteractiveReloadReusesSessionAfterCompletedRead(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDGrepInteractiveReloadReusesSessionAfterCompletedRead")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	inputFile := "interactive_dgrep_reload.tmp"
	if err := os.WriteFile(inputFile, []byte("ERROR first\nWARN second\n"), 0600); err != nil {
		t.Fatalf("unable to create input file: %v", err)
	}
	cleanupFiles(t, inputFile, "interactive_dgrep_reload.stdout.tmp")

	port, serverLogs := startInteractiveTestServer(ctx, t)

	clientOutput, err := runInteractivePTYCommand(ctx, []string{
		"../dgrep",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port),
		"--files", inputFile,
		"--grep", "ERROR",
		"--plain",
		"--trustAllHosts",
		"--interactive-query",
	}, []interactiveStep{
		{At: 3500 * time.Millisecond, Input: ":reload --grep WARN"},
		{At: 5500 * time.Millisecond, Input: ":quit"},
	})
	if err != nil {
		t.Fatalf("run interactive dgrep: %v\noutput:\n%s", err, clientOutput)
	}

	if !strings.Contains(clientOutput, "ERROR first") {
		t.Fatalf("expected initial grep result in output:\n%s", clientOutput)
	}
	if !strings.Contains(clientOutput, "WARN second") {
		t.Fatalf("expected reloaded grep result in output:\n%s", clientOutput)
	}
	if !strings.Contains(clientOutput, "reload applied successfully") {
		t.Fatalf("expected reload success message in output:\n%s", clientOutput)
	}
	if countSubstring(serverLogs.snapshot(), "Creating new server handler") != 1 {
		t.Fatalf("expected one SSH session on the server, logs:\n%s", serverLogs.dump())
	}
}

// TestDGrepInteractiveReloadQuoting checks the shell-like quoting of :reload
// end to end, and that the server keeps the blanks of a regex sent with
// SESSION START or UPDATE:
//
//   - the initial `x  y` regex (two spaces, via SESSION START) and a
//     reloaded 'p  q' must not collapse to `x y` and `p q`;
//   - `foo\ ` keeps its escaped trailing blank, so it matches "foo bar end"
//     but not "foobar end";
//   - a single-quoted regex keeps its backslash (`'\d+ ms'` must not become
//     `d+ ms`, which would match "odd ms" instead of "latency 12 ms");
//   - an empty quoted value clears the regex so every line matches again;
//   - an empty --files value is rejected instead of cancelling the read for
//     nothing.
//
// The needles are data lines which the PTY echo of the typed :reload lines
// does not contain.
func TestDGrepInteractiveReloadQuoting(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDGrepInteractiveReloadQuoting")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	inputFile := "interactive_dgrep_quoting.tmp"
	input := "ERROR first\nx y end\nx  y end\nfoobar end\nfoo bar end\n" +
		"p q end\np  q end\nlatency 12 ms\nodd ms\n"
	if err := os.WriteFile(inputFile, []byte(input), 0600); err != nil {
		t.Fatalf("unable to create input file: %v", err)
	}
	cleanupFiles(t, inputFile, "interactive_dgrep_quoting.stdout.tmp")

	port, _ := startInteractiveTestServer(ctx, t)
	clientOutput, err := runInteractivePTYCommand(ctx, []string{
		"../dgrep", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port), "--files", inputFile,
		"--grep", "x  y", "--plain", "--trustAllHosts", "--interactive-query",
	}, []interactiveStep{
		{WaitFor: "x  y end", Input: `:reload --regex 'p  q'`},
		{WaitFor: "p  q end", Input: `:reload --regex foo\ `},
		{WaitFor: "foo bar end", Input: `:reload --regex '\d+ ms'`},
		{WaitFor: "latency 12 ms", Input: `:reload --regex ''`},
		{WaitFor: "odd ms", Input: `:reload --files ''`},
		{WaitFor: "interactive query error: --files needs at least one non-blank file", Input: ":quit"},
	})
	if err != nil {
		t.Fatalf("run interactive dgrep: %v\noutput:\n%s", err, clientOutput)
	}
	assertInteractiveQuotingOutput(t, clientOutput)
}

// assertInteractiveQuotingOutput checks the output of
// TestDGrepInteractiveReloadQuoting: each line is printed once by the read
// its regex selects and once more by the final cleared-regex read, which
// alone prints the lines no other regex matched.
func assertInteractiveQuotingOutput(t *testing.T, clientOutput string) {
	t.Helper()
	for needle, want := range map[string]int{
		"ERROR first":                 1,
		"x  y end":                    2,
		"x y end":                     1,
		"p  q end":                    2,
		"p q end":                     1,
		"foo bar end":                 2,
		"foobar end":                  1,
		"latency 12 ms":               2,
		"odd ms":                      1,
		"reload applied successfully": 4,
		// The rejected empty --files reload neither applies nor reads again.
		"interactive query error: --files needs at least one non-blank file": 1,
	} {
		if got := strings.Count(clientOutput, needle); got != want {
			t.Fatalf("expected %d x %q in output, got %d:\n%s", want, needle, got, clientOutput)
		}
	}
}

// startInteractiveTestServer starts a dserver (debug logging) on a unique
// port for an interactive test and returns the port once the server accepts
// connections, together with a collector of its output lines, reset at that
// point so it holds only what the test caused.
func startInteractiveTestServer(ctx context.Context, t *testing.T) (int, *processOutputCollector) {
	t.Helper()
	port := getUniquePortNumber()
	args := []string{
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "debug",
		"--bindAddress", "localhost",
		"--port", fmt.Sprintf("%d", port),
	}
	collector := &processOutputCollector{}
	launchInteractiveTestServer(ctx, t, "../dserver", args, collector)
	if err := waitForServerReady(ctx, "localhost", port); err != nil {
		t.Fatalf("wait for dserver: %v", err)
	}
	collector.reset()
	return port, collector
}

// launchInteractiveTestServer starts the server binary cmdStr with args and
// registers a t.Cleanup that stops it again.
//
// The server's stdout and stderr are written straight into the collector by
// os/exec instead of going through startCommand's line channels: nothing can
// block on an unread channel, and the cleanup stops the server and waits for
// it, which also waits for the output copying, so the test leaks neither the
// process nor a goroutine. WaitDelay bounds that wait should a child process
// keep the output pipes open after the server was killed.
func launchInteractiveTestServer(ctx context.Context, t *testing.T, cmdStr string,
	args []string, collector *processOutputCollector) {
	t.Helper()
	if _, err := os.Stat(cmdStr); err != nil {
		t.Fatalf("no such executable '%s', please compile first: %v", cmdStr, err)
	}
	if logger := GetTestLogger(ctx); logger != nil {
		logger.LogCommand(cmdStr, args)
	}
	t.Log(cmdStr, strings.Join(args, " "))

	serverCtx, stopServer := context.WithCancel(ctx)
	cmd := exec.CommandContext(serverCtx, cmdStr, args...)
	cmd.WaitDelay = 5 * time.Second
	// The same comparable writer for both streams makes os/exec serialize
	// the writes, so Write needs no extra care for concurrent callers.
	cmd.Stdout, cmd.Stderr = collector, collector
	if err := cmd.Start(); err != nil {
		stopServer()
		t.Fatalf("start %s: %v", cmdStr, err)
	}
	t.Cleanup(func() {
		stopServer()
		// The exit status is that of a killed process; only the wait matters.
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s output:\n%s", cmdStr, collector.dump())
		}
	})
}

// startProcessOutputCollector collects the lines of startCommand's output
// channels until they close or ctx is done.
func startProcessOutputCollector(ctx context.Context, stdoutCh, stderrCh <-chan string) *processOutputCollector {
	collector := &processOutputCollector{}
	collect := func(ch <-chan string) {
		for {
			select {
			case line, ok := <-ch:
				if !ok {
					return
				}
				collector.append(line)
			case <-ctx.Done():
				return
			}
		}
	}
	go collect(stdoutCh)
	go collect(stderrCh)
	return collector
}

func (c *processOutputCollector) append(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

// Write implements io.Writer for os/exec: it splits the process output into
// lines like bufio.ScanLines (a trailing \r is dropped) and keeps an
// unfinished last line until its newline arrives.
func (c *processOutputCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.partial = append(c.partial, p...)
	rest := c.partial
	for {
		idx := bytes.IndexByte(rest, '\n')
		if idx < 0 {
			break
		}
		c.lines = append(c.lines, string(bytes.TrimSuffix(rest[:idx], []byte("\r"))))
		rest = rest[idx+1:]
	}
	c.partial = append(c.partial[:0], rest...)
	return len(p), nil
}

func (c *processOutputCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// dump returns the collected lines for a failure message, including an
// unfinished last line that Write still holds back for its newline.
func (c *processOutputCollector) dump() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := strings.Join(c.lines, "\n")
	if len(c.partial) > 0 {
		out += "\n" + string(c.partial) + " (unfinished line)"
	}
	return out
}

// reset drops everything collected so far, an unfinished last line included,
// so that the collector holds only output written afterwards.
func (c *processOutputCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = c.lines[:0]
	c.partial = c.partial[:0]
}

func appendLinesOnSchedule(ctx context.Context, path string, steps []interactiveStep) error {
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer closeIgnoringError(fd)

	start := time.Now()
	for _, step := range steps {
		wait := time.Until(start.Add(step.At))
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		if _, err := fd.WriteString(step.Input + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func runInteractivePTYCommand(ctx context.Context, argv []string, steps []interactiveStep) (string, error) {
	if _, err := exec.LookPath("python3"); err != nil {
		return "", fmt.Errorf("python3 is required for PTY integration tests: %w", err)
	}

	script := `
import json
import os
import pty
import sys
import threading
import time

argv = json.loads(os.environ["DTAIL_PTY_ARGV"])
steps = json.loads(os.environ["DTAIL_PTY_STEPS"])

pid, fd = pty.fork()
if pid == 0:
    os.execv(argv[0], argv)

# Shared output buffer, populated by the reader loop below and consulted by the
# writer thread so a step can wait for a marker to appear before sending input.
# Declared before the writer starts so a wait_for step never races the buffer.
output = bytearray()

# Upper bound for how long a wait_for step blocks. The Go-side test context also
# bounds the whole process, so a genuine product hang still fails rather than
# hanging forever; on timeout the input is sent anyway so the pending Go
# assertion reports the real problem instead of the harness deadlocking.
WAIT_FOR_TIMEOUT = 30.0

def writer():
    start = time.monotonic()
    for step in steps:
        wait_for = step.get("wait_for") or ""
        if wait_for:
            needle = wait_for.encode("utf-8")
            deadline = time.monotonic() + WAIT_FOR_TIMEOUT
            while needle not in bytes(output):
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.01)
        wait_for_file = step.get("wait_for_file") or ""
        if wait_for_file:
            deadline = time.monotonic() + WAIT_FOR_TIMEOUT
            while not os.path.exists(wait_for_file):
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.01)
        if not wait_for and not wait_for_file:
            wait = (step["at_ms"] / 1000.0) - (time.monotonic() - start)
            if wait > 0:
                time.sleep(wait)
        data = step["input"]
        if not data.endswith("\n"):
            data += "\n"
        os.write(fd, data.encode("utf-8"))

threading.Thread(target=writer, daemon=True).start()

while True:
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    output.extend(chunk)

_, status = os.waitpid(pid, 0)
sys.stdout.buffer.write(output)
if os.WIFEXITED(status):
    sys.exit(os.WEXITSTATUS(status))
if os.WIFSIGNALED(status):
    sys.exit(128 + os.WTERMSIG(status))
sys.exit(1)
`

	encodedSteps := make([]map[string]any, 0, len(steps))
	for _, step := range steps {
		encodedSteps = append(encodedSteps, map[string]any{
			"at_ms":         step.At.Milliseconds(),
			"wait_for":      step.WaitFor,
			"wait_for_file": step.WaitForFile,
			"input":         step.Input,
		})
	}

	argvPayload, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	stepsPayload, err := json.Marshal(encodedSteps)
	if err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "python3", "-c", script)
	cmd.Env = append(os.Environ(),
		"DTAIL_PTY_ARGV="+string(argvPayload),
		"DTAIL_PTY_STEPS="+string(stepsPayload),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func countSubstring(lines []string, needle string) int {
	count := 0
	for _, line := range lines {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// waitForCollectorCount blocks until at least min lines containing needle have
// been collected, or the context is done. It is used to wait for the Nth
// occurrence of a server log line (e.g. the second "Start reading" emitted when
// a reload dispatches a fresh read command under the new generation).
func waitForCollectorCount(ctx context.Context, collector *processOutputCollector, needle string, min int) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if countSubstring(collector.snapshot(), needle) >= min {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// appendLines appends the given lines (each terminated with a newline) to the
// file at path. Used to feed follow-file content at deterministic points once a
// synchronization barrier has been observed, rather than on a fixed schedule.
func appendLines(path string, lines ...string) error {
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer closeIgnoringError(fd)

	for _, line := range lines {
		if _, err := fd.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func waitForCollectorSubstring(ctx context.Context, collector *processOutputCollector, needle string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		for _, line := range collector.snapshot() {
			if strings.Contains(line, needle) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
