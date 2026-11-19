package integrationtests

import (
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
	// only consulted when WaitFor is empty.
	At time.Duration
	// WaitFor, when set, makes the step block until this substring appears in
	// the accumulated PTY output before Input is sent (bounded by a timeout).
	// This replaces racy fixed-offset timing with deterministic
	// synchronization on observed output.
	WaitFor string
	Input   string
}

type processOutputCollector struct {
	mu    sync.Mutex
	lines []string
}

func TestDTailInteractiveReloadReusesSessionAndDropsLateOldMatches(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDTailInteractiveReloadReusesSessionAndDropsLateOldMatches")
	defer testLogger.WriteLogFile()
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	followFile := "interactive_dtail_reload.tmp"
	if err := os.WriteFile(followFile, nil, 0600); err != nil {
		t.Fatalf("unable to create follow file: %v", err)
	}
	cleanupFiles(t, followFile, "interactive_dtail_reload.stdout.tmp")

	port := getUniquePortNumber()
	serverStdout, serverStderr, _, err := startCommand(ctx, t, "", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "debug",
		"--bindAddress", "localhost",
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Fatalf("start dserver: %v", err)
	}
	serverLogs := startProcessOutputCollector(ctx, serverStdout, serverStderr)
	if err := waitForServerReady(ctx, "localhost", port); err != nil {
		t.Fatalf("wait for dserver: %v", err)
	}
	serverLogs.reset()

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
		t.Fatalf("expected one SSH session on the server, logs:\n%s", strings.Join(serverLogs.snapshot(), "\n"))
	}
}

func TestDTailInteractiveReloadReusesSessionOnImmediateBoundaryAndDropsLateOldMatches(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDTailInteractiveReloadReusesSessionOnImmediateBoundaryAndDropsLateOldMatches")
	defer testLogger.WriteLogFile()
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

	port := getUniquePortNumber()
	serverStdout, serverStderr, _, err := startCommand(ctx, t, "", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "debug",
		"--bindAddress", "localhost",
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Fatalf("start dserver: %v", err)
	}
	serverLogs := startProcessOutputCollector(ctx, serverStdout, serverStderr)
	if err := waitForServerReady(ctx, "localhost", port); err != nil {
		t.Fatalf("wait for dserver: %v", err)
	}
	serverLogs.reset()

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
		t.Fatalf("expected one SSH session on the server, logs:\n%s", strings.Join(serverLogs.snapshot(), "\n"))
	}
}

func TestDGrepInteractiveReloadReusesSessionAfterCompletedRead(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDGrepInteractiveReloadReusesSessionAfterCompletedRead")
	defer testLogger.WriteLogFile()
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	inputFile := "interactive_dgrep_reload.tmp"
	if err := os.WriteFile(inputFile, []byte("ERROR first\nWARN second\n"), 0600); err != nil {
		t.Fatalf("unable to create input file: %v", err)
	}
	cleanupFiles(t, inputFile, "interactive_dgrep_reload.stdout.tmp")

	port := getUniquePortNumber()
	serverStdout, serverStderr, _, err := startCommand(ctx, t, "", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "debug",
		"--bindAddress", "localhost",
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Fatalf("start dserver: %v", err)
	}
	serverLogs := startProcessOutputCollector(ctx, serverStdout, serverStderr)
	if err := waitForServerReady(ctx, "localhost", port); err != nil {
		t.Fatalf("wait for dserver: %v", err)
	}
	serverLogs.reset()

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
		t.Fatalf("expected one SSH session on the server, logs:\n%s", strings.Join(serverLogs.snapshot(), "\n"))
	}
}

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

func (c *processOutputCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

func (c *processOutputCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = c.lines[:0]
}

func appendLinesOnSchedule(ctx context.Context, path string, steps []interactiveStep) error {
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer fd.Close()

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
        else:
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
			"at_ms":    step.At.Milliseconds(),
			"wait_for": step.WaitFor,
			"input":    step.Input,
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
	defer fd.Close()

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
