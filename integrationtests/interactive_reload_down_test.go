package integrationtests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// appendFilterTicks appends an "ERROR tick" and a "WARN tick" line to file every
// 100 ms until the test ends, so that a following dtail prints lines of
// whichever filter is in effect.
func appendFilterTicks(t *testing.T, file string) {
	t.Helper()
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, file)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(f, "ERROR tick %d\nWARN tick %d\n", i, i)
			_ = f.Close()
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })
}

// requireExitedNormally fails unless err is nil or an exit status of the
// client (which exits 1 for a server it could not connect to).
func requireExitedNormally(t *testing.T, err error, output string) {
	t.Helper()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run interactive dtail: %v\noutput:\n%s", err, output)
	}
}

// TestInteractiveReloadWithServerDown: a :reload of dtail with one server
// down (a refused port) next to a healthy one is applied to the healthy one
// and names the server that was not updated, instead of being rejected as a
// whole. :show counts the active sessions.
func TestInteractiveReloadWithServerDown(t *testing.T) {
	skipIfNotIntegrationTest(t)
	testLogger := NewTestLogger("TestInteractiveReloadWithServerDown")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	logFile := "interactive_reload_down.log.tmp"
	appendFilterTicks(t, logFile)
	port, _ := startInteractiveTestServer(ctx, t)
	dead := closedLocalPortAddress(t)
	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--servers", dead + "," + fmt.Sprintf("localhost:%d", port), "--files", logFile,
		"--grep", "ERROR", "--noColor", "--trustAllHosts", "--interactive-query"}
	output, err := runInteractivePTYCommand(ctx, argv, []interactiveStep{
		{WaitFor: "ERROR tick", Input: ":reload --grep WARN"},
		{WaitFor: "reload applied successfully", Input: ":show"},
		{WaitFor: "WARN tick", Input: ":quit"},
	})
	requireExitedNormally(t, err, output)
	if strings.Contains(output, "reload failed") {
		t.Fatalf("reload failed, output:\n%s", output)
	}
	// Between its reconnect attempts the dead server's session ended;
	// during one it is connecting.
	if !strings.Contains(output, "were not updated: "+dead) && !strings.Contains(output, "start with it: "+dead) {
		t.Fatalf("the reload answer does not name %s, output:\n%s", dead, output)
	}
	if !strings.Contains(output, "servers=2") || !strings.Contains(output, "sessions=1") {
		t.Fatalf(":show does not count 2 servers and 1 session, output:\n%s", output)
	}
}

// TestInteractiveReloadServerComesUpLater: a server that is down when dtail
// reloads its filter and comes up afterwards runs the reloaded filter, not
// the one dtail started with: dtail reconnects it with the reloaded spec.
// Its lines are told apart by the server's hostname (late).
func TestInteractiveReloadServerComesUpLater(t *testing.T) {
	skipIfNotIntegrationTest(t)
	testLogger := NewTestLogger("TestInteractiveReloadServerComesUpLater")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)
	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	logFile := "interactive_reload_later.log.tmp"
	appendFilterTicks(t, logFile)
	port, healthy := startInteractiveTestServer(ctx, t)
	latePort := getUniquePortNumber()
	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d,localhost:%d", port, latePort), "--files", logFile,
		"--grep", "ERROR", "--noColor", "--trustAllHosts", "--interactive-query"}

	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := runInteractivePTYCommand(ctx, argv, []interactiveStep{
			{WaitFor: "ERROR tick", Input: ":reload --grep WARN"},
			{WaitFor: "REMOTE|late|", Input: ":quit"},
		})
		done <- result{output, err}
	}()

	// The late server starts once the healthy one got the reload.
	if !waitForCollected(ctx, healthy, "SESSION UPDATE") {
		res := <-done
		t.Fatalf("healthy server got no SESSION UPDATE, server log:\n%s\nclient output:\n%s",
			healthy.dump(), res.output)
	}
	t.Setenv("DTAIL_HOSTNAME_OVERRIDE", "late")
	startInteractiveTestServerOnPort(ctx, t, latePort)

	res := <-done
	requireExitedNormally(t, res.err, res.output)
	var lateWarn bool
	for line := range strings.SplitSeq(res.output, "\n") {
		if !strings.Contains(line, "REMOTE|late|") {
			continue
		}
		if strings.Contains(line, "ERROR tick") {
			t.Fatalf("the late server ran the replaced filter: %q\noutput:\n%s", line, res.output)
		}
		lateWarn = lateWarn || strings.Contains(line, "WARN tick")
	}
	if !lateWarn {
		t.Fatalf("no line of the reloaded filter from the late server, output:\n%s", res.output)
	}
}

// waitForCollected waits until a line collector holds contains want, and
// reports false if ctx ended or 30 s passed first.
func waitForCollected(ctx context.Context, collector *processOutputCollector, want string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if strings.Contains(collector.dump(), want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
