package integrationtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDGrepInteractiveInvertCarriesOver runs --invert through the interactive
// SESSION START/UPDATE path: the initial inverted regex comes from the
// command line, :show reports it, a :reload of the regex alone keeps it (so
// the inverted empty pattern prints no line), and --invert=false matches
// every line again. Each data line is counted at the end: the inverted
// "alpha" read prints the other two lines, the inverted empty read prints
// nothing, and the final positive empty read prints all three once more.
//
// The inverted empty read prints nothing whether it works or never ran, so
// the --invert=false reload waits for the server to have finished it (see
// signalAfterReads); otherwise that reload could cancel it before it read a
// line and a broken read printing every line would go unnoticed. The final
// :show waits for the third read to finish too: a broken second read already
// printed "alpha line", so waiting for that text alone could let :quit cancel
// the third read, and the counts of a broken run (alpha 1, beta 2, gamma 2)
// would then equal those of a working one.
func TestDGrepInteractiveInvertCarriesOver(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDGrepInteractiveInvertCarriesOver")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	inputFile := "interactive_dgrep_invert.tmp"
	if err := os.WriteFile(inputFile, []byte("alpha line\nbeta line\ngamma line\n"), 0600); err != nil {
		t.Fatalf("unable to create input file: %v", err)
	}
	cleanupFiles(t, inputFile)

	port, serverLogs := startInteractiveTestServer(ctx, t)
	readDone := "interactive_dgrep_invert_read_done.tmp"
	cleanupFiles(t, readDone)
	go signalAfterReads(ctx, serverLogs, 2, readDone)
	readDone3 := "interactive_dgrep_invert_read_done3.tmp"
	cleanupFiles(t, readDone3)
	go signalAfterReads(ctx, serverLogs, 3, readDone3)
	clientOutput, err := runInteractivePTYCommand(ctx, []string{
		"../dgrep", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port), "--files", inputFile,
		"--grep", "alpha", "--invert", "--plain", "--trustAllHosts", "--interactive-query",
	}, []interactiveStep{
		{WaitFor: "gamma line", Input: ":show"},
		{WaitFor: `regex="alpha" invert=true`, Input: ":reload --regex ''"},
		{WaitFor: "reload applied successfully", Input: ":show"},
		{WaitFor: `regex="" invert=true`, WaitForFile: readDone, Input: ":reload --regex '' --invert=false"},
		{WaitFor: "alpha line", WaitForFile: readDone3, Input: ":show"},
		{WaitFor: `regex="" invert=false`, Input: ":quit"},
	})
	if err != nil {
		t.Fatalf("run interactive dgrep: %v\noutput:\n%s", err, clientOutput)
	}
	for needle, want := range map[string]int{
		"alpha line":                  1,
		"beta line":                   2,
		"gamma line":                  2,
		"reload applied successfully": 2,
	} {
		if got := strings.Count(clientOutput, needle); got != want {
			t.Fatalf("expected %d x %q in output, got %d:\n%s", want, needle, got, clientOutput)
		}
	}
}

// readsDoneDrain is how long signalAfterReads waits after the last read
// finished: the read flushed its lines into the session output when the
// server logs its end, and they still have to leave the output buffer before
// a reload drops what is left of the old generation.
const readsDoneDrain = 500 * time.Millisecond

// signalAfterReads creates the file marker once the server logged the end of
// reads read commands ("All files processed", logged once per read command,
// also when a reload cancels it) and readsDoneDrain passed, so that a PTY
// step waiting for marker (interactiveStep.WaitForFile) runs after them.
func signalAfterReads(ctx context.Context, serverLogs *processOutputCollector, reads int, marker string) {
	if waitForCollectorCount(ctx, serverLogs, "All files processed", reads) != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(readsDoneDrain):
	}
	// A failed write leaves the step waiting until its timeout, and the
	// test's output assertions report the outcome.
	_ = os.WriteFile(marker, nil, 0600)
}

// TestDTailInvertEmptyRegex follows a file with dtail, an empty --regex and
// --invert in the default command mode, where the client sends the read command
// "regex:invert,literal " that dserver splits at single spaces: it must print
// no line. A second client inverting a regex that never matches runs as a
// control and must print the appended lines, which proves they reached the
// follow sessions.
func TestDTailInvertEmptyRegex(t *testing.T) {
	skipIfNotIntegrationTest(t)

	testLogger := NewTestLogger("TestDTailInvertEmptyRegex")
	defer writeLogFileIgnoringError(testLogger)
	cleanupTmpFiles(t)

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	followFile := "dtail_invert_empty.follow.tmp"
	if err := os.WriteFile(followFile, nil, 0600); err != nil {
		t.Fatalf("unable to create follow file: %v", err)
	}
	cleanupFiles(t, followFile)

	port, serverLogs := startInteractiveTestServer(ctx, t)
	inverted := startDTailFollowClient(ctx, t, port, followFile, "")
	control := startDTailFollowClient(ctx, t, port, followFile, "never-matching-regex")
	// Both sessions share one follow reader of the file. Once the second one
	// joined, the start of each is fixed at the end of the (empty) file, so
	// every line appended from now on reaches both sessions: the inverted one
	// cannot skip the lines by starting after them.
	// The wait is bounded well below the test context, so a session that never
	// joins fails fast with the server logs instead of at the test timeout.
	joinCtx, joinCancel := context.WithTimeout(ctx, 30*time.Second)
	defer joinCancel()
	if err := waitForCollectorCount(joinCtx, serverLogs, sharedFollowGainedLog+"2", 1); err != nil {
		t.Fatalf("both follow sessions did not join the shared follow read within 30s: %v\nserver logs:\n%s",
			err, serverLogs.dump())
	}

	const needle = "invert-follow-line"
	for i := 0; countSubstring(control.snapshot(), needle) < 5; i++ {
		if err := appendLines(followFile, fmt.Sprintf("%s %d", needle, i)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("control client got no lines:\n%s", control.dump())
		case <-time.After(200 * time.Millisecond):
		}
	}
	// Give the inverted client the time of a few more lines to print what a
	// broken inverted empty pattern (a noop) would print.
	time.Sleep(time.Second)
	if got := countSubstring(inverted.snapshot(), needle); got != 0 {
		t.Fatalf("dtail --regex '' --invert printed %d lines, want none:\n%s", got, inverted.dump())
	}
}

// startDTailFollowClient starts a dtail follow client of file with --invert
// and the regex, and returns the collector of its output lines.
func startDTailFollowClient(ctx context.Context, t *testing.T, port int, file, regex string) *processOutputCollector {
	t.Helper()
	stdoutCh, stderrCh, _, err := startCommand(ctx, t, "", "../dtail",
		"--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--servers", fmt.Sprintf("localhost:%d", port), "--files", file,
		"--regex", regex, "--invert", "--trustAllHosts", "--noColor",
	)
	if err != nil {
		t.Fatal(err)
	}
	return startProcessOutputCollector(ctx, stdoutCh, stderrCh)
}
