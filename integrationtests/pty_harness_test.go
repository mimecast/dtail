package integrationtests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests of the PTY harness itself run a shell instead of a DTail binary,
// so they need neither built binaries nor the integration test mode.

func skipWithoutPTYTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"python3", "/bin/sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for the PTY harness tests: %v", tool, err)
		}
	}
	// A sandbox or container may have no PTY devices.
	if out, err := exec.Command("python3", "-I", "-c", "import os; os.openpty()").CombinedOutput(); err != nil {
		t.Skipf("no PTY available for the PTY harness tests: %v: %s", err, out)
	}
}

// TestPTYHarnessCompletesSteps: steps whose WaitFor appears send their input.
func TestPTYHarnessCompletesSteps(t *testing.T) {
	skipWithoutPTYTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output, err := runPTYCommand(ctx, []string{"/bin/sh", "-c", `echo ready; read x; echo "got:$x"`},
		[]interactiveStep{{WaitFor: "ready", Input: "hello"}, {WaitFor: "got:hello"}}, 5*time.Second)
	if err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, output)
	}
	if !strings.Contains(output, "got:hello") {
		t.Fatalf("output lacks the answer:\n%s", output)
	}
}

// TestPTYHarnessFailsWhenProcessExitsBeforeWaitFor: a process that exits
// without printing a step's WaitFor fails the run (it used to pass, the
// remaining steps were dropped silently).
func TestPTYHarnessFailsWhenProcessExitsBeforeWaitFor(t *testing.T) {
	skipWithoutPTYTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output, err := runInteractivePTYCommand(ctx, []string{"/bin/sh", "-c", "echo ready"},
		[]interactiveStep{{WaitFor: "ready", Input: ""}, {WaitFor: "never printed", Input: ":quit"}})
	assertPTYStepFailure(t, err, output, 1, "never printed", "before the process exited")
}

// TestPTYHarnessFailsOnWaitForTimeout: a WaitFor that does not appear within
// the timeout fails the run and kills the process, instead of sending the
// input anyway and waiting for the process to end.
func TestPTYHarnessFailsOnWaitForTimeout(t *testing.T) {
	skipWithoutPTYTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	output, err := runPTYCommand(ctx, []string{"/bin/sh", "-c", "echo ready; sleep 20"},
		[]interactiveStep{{WaitFor: "never printed", Input: ":quit"}}, time.Second)
	assertPTYStepFailure(t, err, output, 0, "never printed", "within 1 s")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the process was not killed on the failure, the run took %v", elapsed)
	}
}

// TestPTYHarnessFailsOnWaitForFileTimeout: the same for WaitForFile.
func TestPTYHarnessFailsOnWaitForFileTimeout(t *testing.T) {
	skipWithoutPTYTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	missing := filepath.Join(t.TempDir(), "never-created")
	output, err := runPTYCommand(ctx, []string{"/bin/sh", "-c", "echo ready; sleep 20"},
		[]interactiveStep{{WaitFor: "ready", WaitForFile: missing, Input: ""}}, time.Second)
	assertPTYStepFailure(t, err, output, 0, "ready", "wait_for_file did not appear")

	if err := os.WriteFile(missing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runPTYCommand(ctx, []string{"/bin/sh", "-c", "echo ready; read x; echo done"},
		[]interactiveStep{{WaitFor: "ready", WaitForFile: missing, Input: ""}, {WaitFor: "done"}}, 5*time.Second)
	if err != nil {
		t.Fatalf("run with the file present: %v\noutput:\n%s", err, output)
	}
}

// TestPTYHarnessKillsProcessOnContextEnd: the end of the test context ends
// the process on the PTY too (killing only python3 left it running) and
// still returns its output so far.
func TestPTYHarnessKillsProcessOnContextEnd(t *testing.T) {
	skipWithoutPTYTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pidFile := filepath.Join(t.TempDir(), "pid")
	output, err := runPTYCommand(ctx, []string{"/bin/sh", "-c", "echo $$ > " + pidFile + "; echo ready; exec sleep 60"},
		[]interactiveStep{{WaitFor: "ready", Input: ""}, {WaitFor: "never printed"}}, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the error of the ended context, got %v", err)
	}
	if !strings.Contains(output, "ready") {
		t.Fatalf("the output so far is missing: %q", output)
	}
	pidText, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var pid int
	if _, scanErr := fmt.Sscan(string(pidText), &pid); scanErr != nil {
		t.Fatal(scanErr)
	}
	// The process is reaped by python3 before it exits, so it is gone.
	if signalErr := syscall.Kill(pid, 0); signalErr == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the process %d on the PTY outlived the harness", pid)
	}
}

func assertPTYStepFailure(t *testing.T, err error, output string, step int, waitFor, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the run passed although step %d (WaitFor %q) could not complete, output:\n%s", step, waitFor, output)
	}
	if !isPTYStepFailure(err) {
		t.Fatalf("error %v is no PTY step failure, output:\n%s", err, output)
	}
	for _, want := range []string{fmt.Sprintf("PTY step %d ", step), waitFor, reason} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}
