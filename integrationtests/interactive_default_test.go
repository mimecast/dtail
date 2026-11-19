package integrationtests

import (
	"os"
	"strings"
	"testing"
	"time"
)

// interactiveBannerPrefix starts the banner the control loop writes to the
// control TTY when interactive query control is on.
const interactiveBannerPrefix = "Interactive query control enabled"

// defaultTick starts the lines appended to the file dtail follows.
const defaultTick = "MARK default tick"

// TestDTailInteractiveByDefaultOnTerminal runs dtail on a PTY without
// --interactive-query: on a terminal interactive query control is its
// default (cli.InteractiveDefault), so the banner is shown and :show and
// :quit are answered.
func TestDTailInteractiveByDefaultOnTerminal(t *testing.T) {
	skipIfNotIntegrationTest(t)
	ctx, cancel := createTestContextWithTimeout(t)
	defer cancel()
	followFile := writeDefaultFollowFile(t)
	appendTicks(ctx, t, followFile, defaultTick)

	argv := []string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--files", followFile, "--grep", "MARK", "--plain", "--noColor"}
	steps := []interactiveStep{
		{WaitFor: interactiveBannerPrefix, Raw: true},
		{WaitFor: defaultTick, Input: ":show"},
		// invert is a key of :show only.
		{WaitFor: "invert=false", Input: ":quit"},
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run dtail: %v\noutput:\n%s", err, output)
	}
}

// TestDTailNoInteractiveOnTerminal: --no-interactive turns the default off on
// a terminal, also when --interactive-query is given too. dtail shows no
// banner and ignores :show; it reads its file until Ctrl-C ends it.
func TestDTailNoInteractiveOnTerminal(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for name, flags := range map[string][]string{
		"no-interactive":                   {"--no-interactive"},
		"no-interactive+interactive-query": {"--no-interactive", "--interactive-query"},
	} {
		t.Run(name, func(t *testing.T) {
			testDTailNoInteractiveOnTerminal(t, flags)
		})
	}
}

func testDTailNoInteractiveOnTerminal(t *testing.T, flags []string) {
	ctx, cancel := createTestContextWithTimeout(t)
	defer cancel()
	followFile := writeDefaultFollowFile(t)
	appendTicks(ctx, t, followFile, defaultTick)

	argv := append([]string{"../dtail", "--cfg", "none", "--logger", "stdout", "--logLevel", "info",
		"--files", followFile, "--grep", "MARK", "--plain", "--noColor"}, flags...)
	steps := []interactiveStep{
		{WaitFor: defaultTick, Input: ":show"},
		// An interactive dtail answers :show within milliseconds; give a
		// wrongly interactive one time to, then end the run with Ctrl-\
		// (the terminal is cooked, so the PTY sends SIGQUIT, which dtail
		// takes as a termination signal at once; Ctrl-C would need a
		// second press in time after the stats it prints first).
		{WaitFor: ":show", Delay: 2 * time.Second, Input: "\x1c", Raw: true},
	}
	output, err := runInteractivePTYCommand(ctx, argv, steps)
	if err != nil {
		t.Fatalf("run dtail: %v\noutput:\n%s", err, output)
	}
	for _, unwanted := range []string{interactiveBannerPrefix, "invert="} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("dtail is interactive with %v, output has %q:\n%s", argv, unwanted, output)
		}
	}
}

// writeDefaultFollowFile creates an empty file for dtail to follow, removed
// after the test.
func writeDefaultFollowFile(t *testing.T) string {
	t.Helper()
	path := "interactive_default.log.tmp"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFiles(t, path)
	return path
}
