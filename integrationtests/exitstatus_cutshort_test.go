package integrationtests

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// cutShortLines is the number of lines of the file the cut short tests read,
// about 40 MB: much more than the buffers between dserver and the test hold
// (the session output buffer, the SSH channel window, the socket and pipe
// buffers), so that the read is still running when the test, which stops
// reading the client's output after its first payload line, stops dserver or
// the client.
const cutShortLines = 400000

// cutShortLookalikes are lines of the file the cut short tests read that
// look like the server's hidden messages: the close handshake and a command
// failure (see the client's baseHandler.handleHiddenMessage and
// protocol.HiddenCommandFailedPrefix). writeCutShortFile writes them ahead of
// the payload lines, so a client reads them before the test stops the run.
// The client asks for marked control frames (protocol.ControlFramesCommand),
// so they reach it as payload: printed, and neither a close handshake that
// would hide a cut short read nor a command failure. They do not contain
// exitStatusLine, so they are not counted as payload lines, and dgrep, which
// greps for that line, does not print them.
var cutShortLookalikes = []string{".syn close connection", ".syn command failed x"}

// cutShortError is the error dcat and dgrep log for a session cut short (see
// clients.baseClient.reportCutShort).
const cutShortError = "Session ended without the server's close handshake"

// ctrlCHint is the hint the client prints for a first SIGINT (see
// signal.runInterruptHandler); it pauses the client's output for
// config.InterruptTimeoutS seconds.
const ctrlCHint = "Hint: Hit Ctrl+C again to exit"

// secondInterruptDelay is how long after the hint the test sends the second
// SIGINT. It balances two margins: the second SIGINT must arrive within the
// interrupt pause (3 s) that the first one started, which leaves 1.5 s, and
// the client, which exits once that pause ended (about 3.2 s after the first
// SIGINT), must exit before signal.forceExitAfter force-exits it 5 s after the
// second one, which leaves about 3.3 s. Sending the pair 200 ms apart left
// only about 1.8 s for the latter.
const secondInterruptDelay = 1500 * time.Millisecond

// cutShortRun is the result of runCutShortClient.
type cutShortRun struct {
	exitCode int
	lines    int    // payload lines of the file in the output
	output   string // the other lines of the output
	// hint is closed once the output contained ctrlCHint.
	hint     chan struct{}
	hintSeen bool
	// sinceSecondInterrupt is how long the client ran after the test sent
	// its second SIGINT, 0 if it sent none.
	sinceSecondInterrupt time.Duration
}

// cutShortControl is what a cutShortStop gets to stop a run with.
type cutShortControl struct {
	killServer func()      // SIGKILLs the dserver and waits until it exited
	client     *os.Process // the client's process
	clientDone <-chan struct{}
	hint       <-chan struct{} // closed once the client printed ctrlCHint
	// secondInterrupt receives the time the second SIGINT was sent (buffered).
	secondInterrupt chan<- time.Time
}

// cutShortStop stops a run once its first payload line arrived. It must not
// block: the test reads the client's output only after it returned.
type cutShortStop func(control cutShortControl)

// TestDCatDGrepExitStatusCutShort checks that dcat and dgrep exit with status
// 1 when dserver is SIGKILLed in the middle of a read, so that the session
// ends without the server's close handshake, with --plain and with framed
// output, also with a forged close handshake line in the file
// (cutShortLookalikes), that the same read exits with status 0 when it
// completes and prints the lookalike lines as payload, and that
// a client stopped by SIGTERM or a double SIGINT keeps exit status 0 and
// stops before the read completed (so a signal the client ignored, which lets
// the read complete with status 0, does not pass).
func TestDCatDGrepExitStatusCutShort(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	testLogger := NewTestLogger("TestDCatDGrepExitStatusCutShort")
	defer writeLogFileIgnoringError(testLogger)
	file := writeCutShortFile(t)

	for _, tc := range cutShortCommands(file) {
		t.Run(tc.name+", server killed", func(t *testing.T) {
			run := runCutShortClient(t, testLogger, tc.cmd, tc.args, killCutShortServer)
			if run.exitCode != 1 || run.lines >= cutShortLines || !strings.Contains(run.output, cutShortError) {
				t.Fatalf("%s exit status = %d with %d of %d lines, want 1 with fewer lines and %q; output:\n%s",
					tc.cmd, run.exitCode, run.lines, cutShortLines, cutShortError, run.output)
			}
		})
	}
	dcatArgs := []string{"--plain", "--files", file}
	t.Run("dcat completed", func(t *testing.T) {
		run := runCutShortClient(t, testLogger, "../dcat", dcatArgs, nil)
		if run.exitCode != 0 || run.lines != cutShortLines {
			t.Fatalf("dcat exit status = %d with %d of %d lines, want 0 with all lines; output:\n%s",
				run.exitCode, run.lines, cutShortLines, run.output)
		}
		for _, lookalike := range cutShortLookalikes {
			if !strings.Contains(run.output, lookalike+"\n") {
				t.Fatalf("dcat output lacks the payload line %q; output:\n%s", lookalike, run.output)
			}
		}
	})
	for name, stop := range map[string]cutShortStop{"SIGTERM": stopCutShortClientTerm,
		"double SIGINT": stopCutShortClientInterrupt} {
		t.Run("dcat stopped by "+name, func(t *testing.T) {
			run := runCutShortClient(t, testLogger, "../dcat", dcatArgs, stop)
			t.Logf("dcat read %d of %d lines", run.lines, cutShortLines)
			if run.sinceSecondInterrupt > 0 {
				t.Logf("dcat exited %v after the second SIGINT", run.sinceSecondInterrupt)
			}
			checkCutShortStopped(t, run)
		})
	}
}

// cutShortCommands are the dcat and dgrep runs reading file whose server the
// test kills.
func cutShortCommands(file string) []exitStatusCase {
	dgrep := []string{"--grep", "exit status", "--files", file}
	return []exitStatusCase{
		{name: "dcat plain", cmd: "../dcat", args: []string{"--plain", "--files", file}},
		{name: "dcat framed", cmd: "../dcat", args: []string{"--files", file}},
		{name: "dgrep plain", cmd: "../dgrep", args: append([]string{"--plain"}, dgrep...)},
		{name: "dgrep framed", cmd: "../dgrep", args: dgrep},
	}
}

// writeCutShortFile writes the file the tests read: the cutShortLookalikes
// lines followed by cutShortLines payload lines.
func writeCutShortFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cutshort.log")
	fd, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(fd)
	for _, lookalike := range cutShortLookalikes {
		if _, err := fmt.Fprintln(writer, lookalike); err != nil {
			t.Fatal(err)
		}
	}
	padding := strings.Repeat("x", 64)
	for i := 1; i <= cutShortLines; i++ {
		if _, err := fmt.Fprintf(writer, "%s %07d %s\n", exitStatusLine, i, padding); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkCutShortStopped checks that a client the test stopped exited with
// status 0 before the read completed, and without the cut-short error.
func checkCutShortStopped(t *testing.T, run cutShortRun) {
	t.Helper()
	if run.exitCode != 0 && run.sinceSecondInterrupt >= 5*time.Second {
		t.Fatalf("dcat exit status = %d %v after the second SIGINT: force-exited by "+
			"signal.forceExitAfter, it did not stop in time; output:\n%s",
			run.exitCode, run.sinceSecondInterrupt, run.output)
	}
	if run.exitCode != 0 || strings.Contains(run.output, cutShortError) {
		t.Fatalf("dcat exit status = %d, want 0 and no %q; output:\n%s",
			run.exitCode, cutShortError, run.output)
	}
	if run.lines >= cutShortLines {
		t.Fatalf("dcat read all %d lines (Ctrl+C hint printed: %v): the signal did not "+
			"stop the read; output:\n%s", run.lines, run.hintSeen, run.output)
	}
}

// killCutShortServer SIGKILLs the dserver while it reads.
func killCutShortServer(control cutShortControl) {
	control.killServer()
}

// stopCutShortClientTerm sends the client SIGTERM.
func stopCutShortClientTerm(control cutShortControl) {
	_ = control.client.Signal(syscall.SIGTERM)
}

// stopCutShortClientInterrupt presses Ctrl+C twice: it sends the client a
// SIGINT and, secondInterruptDelay after the client printed ctrlCHint (which
// pauses its output, so the read cannot complete meanwhile), a second one,
// within the interrupt pause (config.InterruptTimeoutS) that the first one
// started, which stops the client. The first SIGINT is sent only once: should
// the client ignore it (its stats loop not ready to print the hint yet), the
// read completes in about a second, long before a resend could help, and
// checkCutShortStopped reports that the signal did not stop the read and
// whether the hint was printed. Once the second is sent, no further SIGINT
// is: the client stops listening for them, and one more would kill it.
func stopCutShortClientInterrupt(control cutShortControl) {
	_ = control.client.Signal(os.Interrupt)
	go func() {
		select {
		case <-control.hint:
			time.Sleep(secondInterruptDelay)
			control.secondInterrupt <- time.Now()
			_ = control.client.Signal(os.Interrupt)
		case <-control.clientDone:
		}
	}()
}

// runCutShortClient starts a dserver and cmdStr with args reading from it,
// calls stop (if not nil) once the first payload line arrived and the
// client's output is held back, then reads the rest of the output and waits
// for the client to exit.
func runCutShortClient(t *testing.T, logger *TestLogger, cmdStr string, args []string,
	stop cutShortStop) cutShortRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(WithTestLogger(context.Background(), logger), 2*time.Minute)
	defer cancel()
	serverCtx, cancelServer := context.WithCancel(ctx)
	port := getUniquePortNumber()
	exited := startServerB(serverCtx, t, "none", port, "", nil)
	killServer := func() { cancelServer(); <-exited }
	defer killServer()
	if err := waitForServerReady(ctx, "127.0.0.1", port); err != nil {
		t.Fatal(err)
	}

	args = append([]string{"--cfg", "none", "--logLevel", "error", "--noColor",
		"--servers", fmt.Sprintf("127.0.0.1:%d", port), "--trustAllHosts"}, args...)
	cmd, output := startCutShortClient(ctx, t, cmdStr, args)
	reader := bufio.NewReader(output)
	run := cutShortRun{hint: make(chan struct{})}
	for run.lines == 0 {
		line, err := reader.ReadString('\n')
		run.add(line)
		if err != nil {
			t.Fatalf("%s output ended before its first line: %v; output:\n%s", cmdStr, err, run.output)
		}
	}
	clientDone := make(chan struct{})
	secondInterrupt := make(chan time.Time, 1)
	if stop != nil {
		stop(cutShortControl{killServer: killServer, client: cmd.Process,
			clientDone: clientDone, hint: run.hint, secondInterrupt: secondInterrupt})
	}
	run.readAll(t, reader)
	close(clientDone)
	run.exitCode = exitCodeFromError(cmd.Wait())
	select {
	case sent := <-secondInterrupt:
		run.sinceSecondInterrupt = time.Since(sent)
	default:
	}
	return run
}

// startCutShortClient starts the client with its stdout and stderr on a
// pipe, which it returns to read from.
func startCutShortClient(ctx context.Context, t *testing.T, cmdStr string, args []string) (*exec.Cmd, io.Reader) {
	t.Helper()
	if logger := GetTestLogger(ctx); logger != nil {
		logger.LogCommand(cmdStr, args)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	cmd := exec.CommandContext(ctx, cmdStr, args...)
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return cmd, reader
}

// add counts line if it is a payload line of the file and keeps it
// otherwise, closing hint at the first ctrlCHint.
func (r *cutShortRun) add(line string) {
	if strings.Contains(line, exitStatusLine) {
		r.lines++
		return
	}
	if strings.Contains(line, ctrlCHint) && !r.hintSeen {
		r.hintSeen = true
		close(r.hint)
	}
	if line != "" {
		r.output += line
	}
}

// readAll reads the rest of the client's output, until the client exited.
func (r *cutShortRun) readAll(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		r.add(line)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("read client output: %v", err)
		}
	}
}
