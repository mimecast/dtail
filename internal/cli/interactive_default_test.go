package cli

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// parseTailFlags binds the common client flags and --no-interactive on a
// fresh flag set as dtail does, parses argv and applies the interactive
// default with the given terminal and control TTY probes.
func parseTailFlags(t *testing.T, argv []string, onTerminal, ttyUsable bool) (config.Args, []string) {
	t.Helper()
	args := config.Args{Mode: omode.TailClient}
	fs := flag.NewFlagSet("dtail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	BindCommonClientFlags(fs, &args)
	d := BindInteractiveDefault(fs)
	var probed []string
	d.onTerminal = func() bool { return onTerminal }
	d.controlTTYUsable = func(path string) bool {
		probed = append(probed, path)
		return ttyUsable
	}
	if err := fs.Parse(argv); err != nil {
		t.Fatalf("parse %q: %v", argv, err)
	}
	d.Resolve(fs, &args)
	return args, probed
}

// TestResolveInteractive covers dtail's interactive default: on in a terminal
// with a usable control TTY, off otherwise, an explicit --interactive-query
// kept in both directions, and --no-interactive winning over everything.
func TestResolveInteractive(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		onTerminal bool
		ttyUsable  bool
		want       bool
	}{
		{name: "terminal default on", onTerminal: true, ttyUsable: true, want: true},
		{name: "no terminal (pipe, cron, CI)", onTerminal: false, ttyUsable: true, want: false},
		{name: "terminal without usable control TTY", onTerminal: true, ttyUsable: false, want: false},
		{name: "--no-interactive in a terminal", argv: []string{"--no-interactive"},
			onTerminal: true, ttyUsable: true, want: false},
		{name: "--no-interactive wins over --interactive-query",
			argv:       []string{"--interactive-query", "--no-interactive"},
			onTerminal: true, ttyUsable: true, want: false},
		{name: "explicit --interactive-query=false in a terminal",
			argv: []string{"--interactive-query=false"}, onTerminal: true, ttyUsable: true, want: false},
		{name: "explicit --interactive-query without a terminal",
			argv: []string{"--interactive-query"}, onTerminal: false, ttyUsable: false, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, _ := parseTailFlags(t, tt.argv, tt.onTerminal, tt.ttyUsable)
			if args.InteractiveQuery != tt.want {
				t.Fatalf("InteractiveQuery = %v, want %v", args.InteractiveQuery, tt.want)
			}
		})
	}
}

// TestResolveInteractiveProbesConfiguredControlTTY checks that the default
// probes the --control-tty path the control loop will open, and only when
// the terminal check passed and nothing was given explicitly.
func TestResolveInteractiveProbesConfiguredControlTTY(t *testing.T) {
	_, probed := parseTailFlags(t, []string{"--control-tty", "/dev/pts/7"}, true, true)
	if len(probed) != 1 || probed[0] != "/dev/pts/7" {
		t.Fatalf("probed %q, want [/dev/pts/7]", probed)
	}
	for _, argv := range [][]string{{"--interactive-query"}, {"--no-interactive"}} {
		if _, probed := parseTailFlags(t, argv, true, true); len(probed) != 0 {
			t.Fatalf("%q probed the control TTY %q", argv, probed)
		}
	}
	if _, probed := parseTailFlags(t, nil, false, true); len(probed) != 0 {
		t.Fatalf("no terminal, yet probed %q", probed)
	}
}

// TestCanOpenControlTTY uses real files: a readable and writable file opens,
// a missing path does not.
func TestCanOpenControlTTY(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !canOpenControlTTY(path) {
		t.Fatalf("canOpenControlTTY(%q) = false for a writable file", path)
	}
	if canOpenControlTTY(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("canOpenControlTTY of a missing path = true")
	}
}

// TestStdioIsTerminalUnderTest runs under `go test`, whose stdout is a pipe
// or file, never a terminal, so the default must stay off there. A
// redirection from /dev/null (a character device, but no terminal) must not
// count as a terminal either.
func TestStdioIsTerminalUnderTest(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	saved := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = saved }()
	if stdioIsTerminal() {
		t.Fatal("stdioIsTerminal() = true with stdin from /dev/null")
	}
}
