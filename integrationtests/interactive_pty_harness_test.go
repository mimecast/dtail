package integrationtests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type interactiveStep struct {
	// At is a fixed delay (from the writer start) before Input is sent. It is
	// only consulted when WaitFor and WaitForFile are empty.
	At time.Duration
	// WaitFor, when set, makes the step block until this substring appears in
	// the accumulated PTY output before Input is sent. It is an assertion as
	// well as a synchronization point: if the substring does not appear
	// within ptyWaitForTimeout, or the process exits without printing it,
	// the harness kills the process and runInteractivePTYCommand returns an
	// error naming the step, so a test does not need to assert it again.
	WaitFor string
	// WaitForFile, when set, makes the step block (after WaitFor) until this
	// file exists before Input is sent, with the same timeout and failure as
	// WaitFor. The test creates the file once an event outside the PTY output
	// happened, e.g. a server log line, so a step can also wait for the server.
	WaitForFile string
	// Delay, when set, is slept after WaitFor and WaitForFile were met and
	// before Input is sent, e.g. to give a process the time to answer input
	// that it must ignore before the next step ends it.
	Delay time.Duration
	Input string
	// Raw sends Input as typed keys without the line ending, e.g. text and
	// a Tab for the line editor's completion.
	Raw bool
}

// ptyWaitForTimeout bounds how long one step waits for its WaitFor or
// WaitForFile before the harness fails the run.
const ptyWaitForTimeout = 30 * time.Second

// ptyHarnessFailurePrefix starts the stderr line the PTY script writes when a
// step failed; the rest of the line is the JSON of a ptyStepFailure.
const ptyHarnessFailurePrefix = "PTY-HARNESS-FAILURE "

// ptyStepFailure describes the step the PTY script could not complete.
type ptyStepFailure struct {
	Step        int    `json:"step"`
	WaitFor     string `json:"wait_for"`
	WaitForFile string `json:"wait_for_file"`
	Reason      string `json:"reason"`
}

func (f ptyStepFailure) Error() string {
	return fmt.Sprintf("PTY step %d (wait_for %q, wait_for_file %q): %s",
		f.Step, f.WaitFor, f.WaitForFile, f.Reason)
}

// ptyHarnessScript runs argv on a PTY, sends the steps' input from a writer
// thread and prints everything the process wrote to the PTY on stdout. A step
// that can not be completed (its WaitFor or WaitForFile did not appear within
// the timeout or before the process exited, its input could not be written,
// or the step raised an exception) is reported on stderr with ptyHarnessFailurePrefix and the process
// is killed, so the run fails at once instead of sending input that the
// scenario did not reach.
const ptyHarnessScript = `
import json
import os
import pty
import signal
import sys
import threading
import time
import traceback

argv = json.loads(os.environ["DTAIL_PTY_ARGV"])
steps = json.loads(os.environ["DTAIL_PTY_STEPS"])
WAIT_FOR_TIMEOUT = float(os.environ["DTAIL_PTY_WAIT_TIMEOUT"])
FAILURE_PREFIX = os.environ["DTAIL_PTY_FAILURE_PREFIX"]

pid, fd = pty.fork()
if pid == 0:
    os.execv(argv[0], argv)

# Shared output buffer, populated by the reader loop below and consulted by the
# writer thread so a step can wait for a marker to appear before sending input.
output = bytearray()
# Set once the reader loop saw the end of the output, so output is complete.
# It is set under kill_lock and the process is reaped only after that, so
# kill_child never signals a reaped (and maybe reused) pid. An RLock, as the
# SIGTERM handler runs in the main thread.
exited = threading.Event()
kill_lock = threading.RLock()

def kill_child():
    with kill_lock:
        if exited.is_set():
            return
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            pass

# The Go side sends SIGTERM when the test context ends: kill the process
# instead of leaving it running without its PTY reader, and print what it
# wrote so far as on every other end.
signal.signal(signal.SIGTERM, lambda signum, frame: kill_child())

def fail(index, step, reason):
    sys.stderr.write(FAILURE_PREFIX + json.dumps({
        "step": index,
        "wait_for": step.get("wait_for") or "",
        "wait_for_file": step.get("wait_for_file") or "",
        "reason": reason,
    }) + "\n")
    sys.stderr.flush()
    kill_child()

# wait_until polls ready until it holds; it returns an error text when the
# timeout passed or the process exited first (ready is checked once more
# after the exit, as the output is complete then).
def wait_until(ready, what):
    deadline = time.monotonic() + WAIT_FOR_TIMEOUT
    while not ready():
        if exited.is_set():
            return None if ready() else what + " did not appear before the process exited"
        if time.monotonic() >= deadline:
            return what + " did not appear within %.0f s" % WAIT_FOR_TIMEOUT
        time.sleep(0.01)
    return None

def run_step(index, step, start):
    wait_for = step.get("wait_for") or ""
    wait_for_file = step.get("wait_for_file") or ""
    error = None
    if wait_for:
        needle = wait_for.encode("utf-8")
        error = wait_until(lambda: needle in output, "wait_for")
    if error is None and wait_for_file:
        error = wait_until(lambda: os.path.exists(wait_for_file), "wait_for_file")
    if error is not None:
        return error
    if not wait_for and not wait_for_file:
        wait = (step["at_ms"] / 1000.0) - (time.monotonic() - start)
        if wait > 0:
            time.sleep(wait)
    if step["delay_ms"] > 0:
        time.sleep(step["delay_ms"] / 1000.0)
    data = step["input"]
    if not step.get("raw") and not data.endswith("\n"):
        data += "\n"
    try:
        os.write(fd, data.encode("utf-8"))
    except OSError as err:
        return "unable to send the input: %s" % err
    return None

def writer():
    start = time.monotonic()
    for index, step in enumerate(steps):
        try:
            error = run_step(index, step, start)
        except Exception:
            # E.g. a malformed step: fail now rather than after a timeout.
            error = "harness error: " + traceback.format_exc()
        if error is not None:
            fail(index, step, error)
            return

writer_thread = threading.Thread(target=writer, daemon=True)
writer_thread.start()

while True:
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    output.extend(chunk)
with kill_lock:
    exited.set()

_, status = os.waitpid(pid, 0)
# The writer returns promptly once exited is set, so a step still waiting
# reports its failure before the exit status is chosen.
writer_thread.join()
sys.stdout.buffer.write(output)
if os.WIFEXITED(status):
    sys.exit(os.WEXITSTATUS(status))
if os.WIFSIGNALED(status):
    sys.exit(128 + os.WTERMSIG(status))
sys.exit(1)
`

// runInteractivePTYCommand runs argv on a PTY (python3 pty), sends the steps'
// input and returns what the process wrote to the PTY. A step whose WaitFor or
// WaitForFile never appeared fails the run with a ptyStepFailure error.
func runInteractivePTYCommand(ctx context.Context, argv []string, steps []interactiveStep) (string, error) {
	return runPTYCommand(ctx, argv, steps, ptyWaitForTimeout)
}

// runPTYCommand is runInteractivePTYCommand with the step timeout as a
// parameter, so the harness tests can fail a step quickly.
func runPTYCommand(ctx context.Context, argv []string, steps []interactiveStep,
	waitTimeout time.Duration) (string, error) {

	if _, err := exec.LookPath("python3"); err != nil {
		return "", fmt.Errorf("python3 is required for PTY integration tests: %w", err)
	}
	env, err := ptyHarnessEnv(argv, steps, waitTimeout)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "python3", "-I", "-c", ptyHarnessScript)
	// On the end of ctx the script kills the process on the PTY (SIGKILL of
	// python3 would leave it running); WaitDelay bounds a script that hangs.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	output := stdout.String()
	if ctx.Err() != nil {
		// The killed process also fails the step it was at; the cause is
		// the ended context.
		return output, fmt.Errorf("PTY run ended with its context: %w (%v, stderr: %s)",
			ctx.Err(), runErr, stderr.String())
	}
	if failure, ok := parsePTYStepFailure(stderr.String()); ok {
		return output, failure
	}
	if runErr != nil && stderr.Len() > 0 {
		return output, fmt.Errorf("%w, stderr: %s", runErr, stderr.String())
	}
	return output, runErr
}

// ptyHarnessEnv returns the environment that passes argv, the steps and the
// harness settings to ptyHarnessScript.
func ptyHarnessEnv(argv []string, steps []interactiveStep, waitTimeout time.Duration) ([]string, error) {
	encodedSteps := make([]map[string]any, 0, len(steps))
	for _, step := range steps {
		encodedSteps = append(encodedSteps, map[string]any{
			"at_ms":         step.At.Milliseconds(),
			"wait_for":      step.WaitFor,
			"wait_for_file": step.WaitForFile,
			"delay_ms":      step.Delay.Milliseconds(),
			"input":         step.Input,
			"raw":           step.Raw,
		})
	}
	argvPayload, err := json.Marshal(argv)
	if err != nil {
		return nil, err
	}
	stepsPayload, err := json.Marshal(encodedSteps)
	if err != nil {
		return nil, err
	}
	return []string{
		"DTAIL_PTY_ARGV=" + string(argvPayload),
		"DTAIL_PTY_STEPS=" + string(stepsPayload),
		fmt.Sprintf("DTAIL_PTY_WAIT_TIMEOUT=%g", waitTimeout.Seconds()),
		"DTAIL_PTY_FAILURE_PREFIX=" + ptyHarnessFailurePrefix,
	}, nil
}

// parsePTYStepFailure returns the step failure the PTY script reported on
// stderr, if any.
func parsePTYStepFailure(stderr string) (ptyStepFailure, bool) {
	for _, line := range strings.Split(stderr, "\n") {
		payload, ok := strings.CutPrefix(line, ptyHarnessFailurePrefix)
		if !ok {
			continue
		}
		var failure ptyStepFailure
		if err := json.Unmarshal([]byte(payload), &failure); err != nil {
			failure = ptyStepFailure{Step: -1, Reason: "unparsable failure report: " + payload}
		}
		return failure, true
	}
	return ptyStepFailure{}, false
}

// isPTYStepFailure reports whether err is a step failure of the PTY harness.
func isPTYStepFailure(err error) bool {
	var failure ptyStepFailure
	return errors.As(err, &failure)
}
