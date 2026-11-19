package clients

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

var (
	completeGrepArgs = config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR|WARN"}
	completeMapArgs  = config.Args{Mode: omode.MapClient, What: "/var/log/a.log,/var/log/b.log",
		QueryStr: "from STATS select count($line) group by $hostname"}
	completeCatArgs = config.Args{Mode: omode.CatClient, What: "/var/log/app.log"}
)

// TestCompleteInteractive covers Tab on control lines: commands, mode flags,
// current values, common prefixes, candidate lists, and the cases that must
// leave the line alone.
func TestCompleteInteractive(t *testing.T) {
	tests := []struct {
		name           string
		args           config.Args
		line           string
		pos            int // -1: end of line
		wantLine       string
		wantCandidates []string
	}{
		{name: "command", args: completeGrepArgs, line: ":re", pos: -1, wantLine: ":reload "},
		{name: "command quit", args: completeCatArgs, line: ":q", pos: -1, wantLine: ":quit"},
		{name: "command query or quit", args: completeGrepArgs, line: ":q", pos: -1, wantLine: ":qu"},
		{name: "command list on bare colon", args: completeCatArgs, line: ":", pos: -1, wantLine: ":",
			wantCandidates: []string{":reload ", ":show", ":fields ", ":probe", ":help", ":quit"}},
		{name: "dtail command list on bare colon", args: completeGrepArgs, line: ":", pos: -1, wantLine: ":",
			wantCandidates: []string{":reload ", ":show", ":fields ", ":probe", ":help", ":quit", ":query ", ":tail "}},
		{name: "unknown command prefix", args: completeGrepArgs, line: ":x", pos: -1, wantLine: ":x"},
		{name: "grep flag", args: completeGrepArgs, line: ":reload --gr", pos: -1, wantLine: ":reload --grep "},
		{name: "single dash", args: completeGrepArgs, line: ":reload -bef", pos: -1, wantLine: ":reload --before "},
		{name: "bool flag common prefix", args: completeGrepArgs, line: ":reload --inv", pos: -1,
			wantLine: ":reload --invert"},
		{name: "bool flag variants listed", args: completeGrepArgs, line: ":reload --invert", pos: -1,
			wantLine: ":reload --invert", wantCandidates: []string{"--invert ", "--invert=false"}},
		{name: "grep value", args: completeGrepArgs, line: ":reload --grep ", pos: -1,
			wantLine: ":reload --grep 'ERROR|WARN'"},
		{name: "regex value single dash", args: completeGrepArgs, line: ":reload -regex ", pos: -1,
			wantLine: ":reload -regex 'ERROR|WARN'"},
		{name: "query value", args: completeMapArgs, line: ":reload --query ", pos: -1,
			wantLine: ":reload --query 'from STATS select count($line) group by $hostname'"},
		{name: "files value unquoted", args: completeMapArgs, line: ":reload --files ", pos: -1,
			wantLine: ":reload --files /var/log/a.log,/var/log/b.log"},
		{name: "flags after reload", args: completeCatArgs, line: ":reload ", pos: -1,
			wantLine: ":reload --", wantCandidates: nil},
		{name: "mid line completes word before cursor", args: completeGrepArgs, line: ":reload --gr x",
			pos: len(":reload --gr"), wantLine: ":reload --grep  x"},
		// Negative cases: no flag of another mode, no value of a flag the
		// mode lacks, no completion inside quotes or after other commands.
		{name: "dmap has no grep", args: completeMapArgs, line: ":reload --gr", pos: -1, wantLine: ":reload --gr"},
		{name: "dcat has no query", args: completeCatArgs, line: ":reload --que", pos: -1, wantLine: ":reload --que"},
		{name: "query mode hides invert", args: completeMapArgs, line: ":reload --inv", pos: -1,
			wantLine: ":reload --inv"},
		{name: "no value of foreign flag", args: completeMapArgs, line: ":reload --grep ", pos: -1,
			wantLine: ":reload --grep --",
		},
		{name: "open quote", args: completeGrepArgs, line: ":reload --grep 'ab --", pos: -1,
			wantLine: ":reload --grep 'ab --"},
		{name: "quoted word", args: completeGrepArgs, line: ":reload --grep 'x", pos: -1,
			wantLine: ":reload --grep 'x"},
		{name: "not after show", args: completeGrepArgs, line: ":show --gr", pos: -1, wantLine: ":show --gr"},
		{name: "reloadx is no reload", args: completeGrepArgs, line: ":reloadx --gr", pos: -1,
			wantLine: ":reloadx --gr"},
		{name: "position out of range", args: completeGrepArgs, line: ":re", pos: 9, wantLine: ":re"},
		// An escaped blank belongs to the value: no new word starts there.
		{name: "escaped blank", args: completeGrepArgs, line: `:reload --regex foo\ `, pos: -1,
			wantLine: `:reload --regex foo\ `},
		{name: "escaped backslash then blank", args: completeGrepArgs, line: `:reload --regex foo\\ `, pos: -1,
			wantLine: `:reload --regex foo\\ --`},
		// After a flag expecting a number nothing is inserted.
		{name: "int flag value", args: completeGrepArgs, line: ":reload --max ", pos: -1,
			wantLine: ":reload --max "},
		{name: "after bool flag the flags", args: completeGrepArgs, line: ":reload --invert ", pos: -1,
			wantLine: ":reload --invert --"},
		// An empty current value is not inserted ('' would be rejected).
		{name: "empty value", args: config.Args{Mode: omode.GrepClient, What: "/x"}, line: ":reload --grep ",
			pos: -1, wantLine: ":reload --grep "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos := tt.pos
			if pos < 0 {
				pos = len(tt.line)
			}
			got := completeInteractive(tt.line, pos, tt.args, emptyCatalog)
			if got.line != tt.wantLine {
				t.Fatalf("line = %q, want %q", got.line, tt.wantLine)
			}
			if tt.wantCandidates != nil && !slices.Equal(got.candidates, tt.wantCandidates) {
				t.Fatalf("candidates = %q, want %q", got.candidates, tt.wantCandidates)
			}
			if got.changed != (got.line != tt.line) {
				t.Fatalf("changed = %v for %q -> %q", got.changed, tt.line, got.line)
			}
			if got.changed && got.pos > len(got.line) {
				t.Fatalf("pos %d beyond line %q", got.pos, got.line)
			}
		})
	}
}

// TestInteractiveReloadFlagsParse checks that every flag completion offers
// is accepted by the :reload parser of its mode, and that each mode offers
// its characteristic flags.
func TestInteractiveReloadFlagsParse(t *testing.T) {
	values := map[string]string{"files": "/var/log/x.log", "grep": "x", "regex": "x",
		"query":  "from STATS select count($line) group by $hostname",
		"before": "1", "after": "1", "max": "1", "timeout": "1"}
	for _, tt := range []struct {
		args config.Args
		want []string
	}{
		{completeGrepArgs, []string{"--grep ", "--regex ", "--invert=false", "--before ", "--files "}},
		{completeMapArgs, []string{"--query ", "--files ", "--timeout "}},
		{completeCatArgs, []string{"--files ", "--plain ", "--quiet "}},
	} {
		flags := interactiveReloadFlags(tt.args)
		for _, want := range tt.want {
			if !slices.Contains(flags, want) {
				t.Fatalf("mode %s: flags %q lack %q", tt.args.Mode, flags, want)
			}
		}
		for _, f := range flags {
			tokens := []string{strings.TrimSpace(f)}
			if v, ok := values[strings.TrimPrefix(strings.TrimSpace(f), "--")]; ok {
				tokens = append(tokens, v)
			}
			if _, err := parseInteractiveReloadArgs(tt.args, tokens); err != nil {
				t.Fatalf("mode %s: completed flag %q rejected: %v", tt.args.Mode, tokens, err)
			}
		}
	}
	if flags := interactiveReloadFlags(config.Args{Mode: omode.HealthClient}); flags != nil {
		t.Fatalf("unsupported mode offers flags %q", flags)
	}
}

// TestQuoteInteractiveArgRoundTrip checks that every inserted value comes
// back unchanged from the :reload tokenizer, special characters included.
func TestQuoteInteractiveArgRoundTrip(t *testing.T) {
	for _, value := range []string{"", "ERROR", "/var/log/a.log,/var/log/b.log", "a b", "it's",
		`\d+ ms`, `"quoted"`, "$line", "tab\there", "x  y", "'", `\`, "ünïcödé"} {
		quoted := quoteInteractiveArg(value)
		tokens, err := splitInteractiveArgs(quoted)
		if err != nil || len(tokens) != 1 || tokens[0] != value {
			t.Fatalf("value %q quoted %q split to %q (err %v)", value, quoted, tokens, err)
		}
	}
	if got := quoteInteractiveArg("plain.log"); got != "plain.log" {
		t.Fatalf("plain value quoted as %q", got)
	}
}

// TestControlHistory checks that blank lines and repeats of the latest line
// are not remembered, the most recent line is At(0), and the size is bounded.
func TestControlHistory(t *testing.T) {
	h := &controlHistory{}
	for _, line := range []string{":show", "", "  ", ":reload --grep x", ":reload --grep x", ":show"} {
		h.Add(line)
	}
	want := []string{":show", ":reload --grep x", ":show"}
	if h.Len() != len(want) {
		t.Fatalf("len = %d, want %d (%q)", h.Len(), len(want), h.entries)
	}
	for i, w := range want {
		if h.At(i) != w {
			t.Fatalf("At(%d) = %q, want %q", i, h.At(i), w)
		}
	}
	for i := 0; i < controlHistoryMax+10; i++ {
		h.Add(strings.Repeat("x", i+1))
	}
	if h.Len() != controlHistoryMax {
		t.Fatalf("len = %d, want bounded %d", h.Len(), controlHistoryMax)
	}
}

// TestNewControlLinesFallsBackWithoutTerminal checks that a control "TTY"
// that is no terminal (a regular file) gets plain line reading, unchanged
// from before the line editor: lines as written, Tab kept as a character,
// then io.EOF.
func TestNewControlLinesFallsBackWithoutTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(path, []byte(":show\n:reload --gr\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tty, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	client := newPauseTestClient(newPausingTestLogger())
	lines := client.newControlLines(tty, client.newInteractivePause(), nil, testEditorDeps())
	defer lines.close()
	if _, ok := lines.(*scannerLines); !ok {
		t.Fatalf("control lines = %T, want *scannerLines", lines)
	}
	for _, want := range []string{":show", ":reload --gr\t"} {
		if line, err := lines.ReadLine(); err != nil || line != want {
			t.Fatalf("ReadLine = %q, %v; want %q", line, err, want)
		}
	}
	if _, err := lines.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine at end: %v, want io.EOF", err)
	}
}

// testEditorDeps are editor dependencies for tests: no forced-exit hook is
// registered with the process (parallel tests would overwrite each other's),
// no environment is read, so the external editor is the fallback, and no
// signal is watched: SIGCONT and SIGWINCH go to the whole test process, so
// the test of one editor's signals would have every other editor write to
// its terminal (bracketed paste on again, a redraw). A test of the signals
// sets watchSignals to watchTTYSignals.
func testEditorDeps() editorDeps {
	return editorDeps{
		setForceExitHook: func(func()) {},
		getenv:           func(string) string { return "" },
		watchSignals:     func(func(), func()) func() { return func() {} },
	}
}

// TestEditorDepsDefaults: dependencies left nil are the process's, so the
// zero value is safe to pass, and set ones are kept.
func TestEditorDepsDefaults(t *testing.T) {
	deps := editorDeps{}.withDefaults()
	if deps.setForceExitHook == nil || deps.getenv == nil || deps.watchSignals == nil {
		t.Fatalf("zero dependencies left nil: %+v", deps)
	}
	process := processEditorDeps()
	if process.setForceExitHook == nil || process.getenv == nil || process.watchSignals == nil {
		t.Fatalf("process dependencies left nil: %+v", process)
	}
	kept := editorDeps{getenv: func(string) string { return "kept" }}.withDefaults()
	if kept.getenv("EDITOR") != "kept" {
		t.Fatal("a set dependency was replaced")
	}
}

// TestCompleteCurrentValueWithControlCharacter: Tab after a value flag
// inserts the current value, but not one holding a control or format
// character (a tab or line break given on the command line, U+202E): the
// Terminal would write it raw and miscount the columns. The line stays as
// it is and a hint says why; a printable value is still inserted.
func TestCompleteCurrentValueWithControlCharacter(t *testing.T) {
	for flagName, set := range map[string]func(*config.Args, string){
		"--regex": func(a *config.Args, v string) { a.RegexStr = v },
		"--grep":  func(a *config.Args, v string) { a.RegexStr = v },
		"--files": func(a *config.Args, v string) { a.What = v },
	} {
		line := ":reload " + flagName + " "
		for _, value := range []string{"a\tb", "a\nb", "a\x1b[2Jb", "a" + string(rune(0x202e)) + "b",
			"a" + string(rune(0x2028)) + "b"} {
			args := completeGrepArgs
			set(&args, value)
			got := completeInteractive(line, len(line), args, nil)
			if got.line != line || got.pos != len(line) || got.changed || len(got.candidates) != 0 {
				t.Errorf("%s with value %q: completion %+v, want the line unchanged", flagName, value, got)
			}
			if !strings.Contains(got.hint, flagName) || hasHiddenRune(got.hint) {
				t.Errorf("%s with value %q: hint %q, want one naming the flag", flagName, value, got.hint)
			}
		}
		args := completeGrepArgs
		set(&args, "it's ok")
		got := completeInteractive(line, len(line), args, nil)
		if want := line + `'it'\''s ok'`; got.line != want || got.hint != "" {
			t.Errorf("%s with a printable value: %+v, want line %q and no hint", flagName, got, want)
		}
	}
}
