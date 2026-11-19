package clients

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"

	"golang.org/x/crypto/ssh"
)

func TestParseInteractiveCommandForGrepReload(t *testing.T) {
	current := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}

	command, err := parseInteractiveCommand(current,
		`:reload --grep WARN --before 2 --after 3 --max 4 --invert --files "/tmp/other.log" --plain --quiet --timeout 7`,
		logging.NopLogger{})
	if err != nil {
		t.Fatalf("parseInteractiveCommand() error = %v", err)
	}

	if command.kind != "reload" {
		t.Fatalf("command kind = %q, want reload", command.kind)
	}
	if command.next.What != "/tmp/other.log" {
		t.Fatalf("files = %q, want /tmp/other.log", command.next.What)
	}
	if command.next.RegexStr != "WARN" {
		t.Fatalf("regex = %q, want WARN", command.next.RegexStr)
	}
	if !command.next.RegexInvert || !command.next.Plain || !command.next.Quiet {
		t.Fatalf("expected invert/plain/quiet flags to be set: %#v", command.next)
	}
	if command.next.BeforeContext != 2 || command.next.AfterContext != 3 || command.next.MaxCount != 4 {
		t.Fatalf("unexpected context values: %#v", command.next.LContext)
	}
	if command.spec.Regex != "WARN" {
		t.Fatalf("session spec regex = %q, want WARN", command.spec.Regex)
	}
}

func TestParseInteractiveCommandForMapReloadDerivesRegex(t *testing.T) {
	current := config.Args{
		Mode:     omode.MapClient,
		What:     "/var/log/app.log",
		QueryStr: "select count(status) from stats group by status",
		// A stale filter of the previous query and an --invert, which a
		// query ignores, must not survive the reload.
		RegexStr:    "\\|MAPREDUCE:STATS\\|",
		RegexInvert: true,
	}

	command, err := parseInteractiveCommand(current,
		`:reload --query "select count(status) from warnings group by status" --files /tmp/new.log --plain --timeout 9`,
		logging.NopLogger{})
	if err != nil {
		t.Fatalf("parseInteractiveCommand() error = %v", err)
	}

	if command.kind != "reload" {
		t.Fatalf("command kind = %q, want reload", command.kind)
	}
	if command.next.QueryStr != "select count(status) from warnings group by status" {
		t.Fatalf("query = %q", command.next.QueryStr)
	}
	if command.spec.Regex != "\\|MAPREDUCE:WARNINGS\\|" {
		t.Fatalf("session spec regex = %q, want WARNINGS table regex", command.spec.Regex)
	}
	// The reloaded args carry the same filter as the spec, never inverted,
	// as applyInteractiveReload stores them for reconnects and :show.
	if command.next.RegexStr != command.spec.Regex || command.next.RegexInvert || command.spec.RegexInvert {
		t.Fatalf("reloaded args = regex %q invert %v (spec invert %v), want %q not inverted",
			command.next.RegexStr, command.next.RegexInvert, command.spec.RegexInvert, command.spec.Regex)
	}
}

// TestParseInteractiveReloadRejectsQueryInvert pins that a :reload of a query
// session (dmap, and dtail started with --query) tells why it does not take
// --invert instead of dropping it or failing with the flag package's generic
// "flag provided but not defined", while --invert=false, which changes
// nothing, is accepted.
func TestParseInteractiveReloadRejectsQueryInvert(t *testing.T) {
	for _, mode := range []omode.Mode{omode.MapClient, omode.TailClient} {
		current := config.Args{
			Mode:     mode,
			What:     "/var/log/app.log",
			QueryStr: "select count(status) from stats group by status",
			RegexStr: "\\|MAPREDUCE:STATS\\|",
		}
		for _, line := range []string{":reload --invert --files /tmp/new.log", ":reload --invert=true --plain"} {
			_, err := parseInteractiveCommand(current, line, logging.NopLogger{})
			if !errors.Is(err, errReloadQueryInvert) {
				t.Errorf("%s %q: error = %v, want %v", mode, line, err, errReloadQueryInvert)
			}
		}
		command, err := parseInteractiveCommand(current, ":reload --invert=false --plain", logging.NopLogger{})
		if err != nil {
			t.Fatalf("%s: --invert=false error = %v", mode, err)
		}
		if command.next.RegexInvert || command.spec.RegexInvert {
			t.Fatalf("%s: --invert=false reload inverts the query filter", mode)
		}
	}
}

func TestParseInteractiveCommandRejectsUnterminatedQuotes(t *testing.T) {
	current := config.Args{
		Mode:     omode.MapClient,
		QueryStr: "select count(status) from stats group by status",
	}

	if _, err := parseInteractiveCommand(current,
		`:reload --query "select count(status) from stats`, logging.NopLogger{}); err == nil {
		t.Fatalf("expected parseInteractiveCommand() to reject unterminated quoted input")
	}
}

func TestSplitInteractiveArgs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "plain words", raw: `--grep WARN --max 4`, want: []string{"--grep", "WARN", "--max", "4"}},
		{name: "spaces and tabs", raw: "  --grep \t WARN  ", want: []string{"--grep", "WARN"}},
		{name: "single quotes keep backslash", raw: `--regex '\d+'`, want: []string{"--regex", `\d+`}},
		{name: "single quotes keep double quote", raw: `--regex 'say "hi"'`, want: []string{"--regex", `say "hi"`}},
		{name: "single quotes end at quote after backslash", raw: `--regex 'a\'`, want: []string{"--regex", `a\`}},
		{name: "double quotes keep other backslashes", raw: `--regex "a\.b\d"`, want: []string{"--regex", `a\.b\d`}},
		{name: "double quotes escape backslash", raw: `--regex "a\\b"`, want: []string{"--regex", `a\b`}},
		{name: "double quotes escape double quote", raw: `--regex "say \"hi\""`, want: []string{"--regex", `say "hi"`}},
		{name: "double quotes keep single quote", raw: `--regex "it's"`, want: []string{"--regex", "it's"}},
		{name: "double quotes keep dollar", raw: `--query "select count($line)"`, want: []string{"--query", "select count($line)"}},
		{name: "double quotes escape dollar", raw: `--regex "\$x\$"`, want: []string{"--regex", "$x$"}},
		{name: "double quotes escape backtick", raw: "--regex \"a\\`b\"", want: []string{"--regex", "a`b"}},
		{name: "single quotes keep escaped dollar", raw: `--regex '\$x'`, want: []string{"--regex", `\$x`}},
		// The tokenizer keeps invalid UTF-8; parseInteractiveReload rejects
		// such a line before it gets here.
		{name: "invalid utf-8 is kept", raw: "--regex \xff'\xfe' \"\xfd\"", want: []string{"--regex", "\xff\xfe", "\xfd"}},
		{name: "unquoted backslash before multibyte", raw: `--files /tmp/\é.log`, want: []string{"--files", "/tmp/é.log"}},
		{name: "double-quoted backslash before multibyte", raw: `--regex "\é"`, want: []string{"--regex", `\é`}},
		{name: "unquoted backslash before invalid byte", raw: "--regex \\\xff", want: []string{"--regex", "\xff"}},
		{name: "unquoted backslash escapes space", raw: `--files /a\ b`, want: []string{"--files", "/a b"}},
		{name: "unquoted backslash escapes quote", raw: `--regex it\'s`, want: []string{"--regex", "it's"}},
		{name: "escaped trailing space", raw: `--regex foo\ `, want: []string{"--regex", "foo "}},
		{name: "escaped trailing tab", raw: "--regex foo\\\t", want: []string{"--regex", "foo\t"}},
		{name: "unquoted backslash escapes backslash", raw: `--regex a\\d`, want: []string{"--regex", `a\d`}},
		{name: "empty single quotes", raw: `--regex ''`, want: []string{"--regex", ""}},
		{name: "empty double quotes", raw: `--regex ""`, want: []string{"--regex", ""}},
		{name: "empty value before flag", raw: `--regex '' --max 1`, want: []string{"--regex", "", "--max", "1"}},
		{name: "empty value after equals", raw: `--regex=''`, want: []string{"--regex="}},
		{name: "adjacent parts join", raw: `--files=/a' 'b"c"d`, want: []string{"--files=/a bcd"}},
		{name: "adjacent empty quotes join", raw: `foo''bar`, want: []string{"foobar"}},
		{name: "empty input", raw: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitInteractiveArgs(tt.raw)
			if err != nil {
				t.Fatalf("splitInteractiveArgs(%q) error = %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("splitInteractiveArgs(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSplitInteractiveArgsRejectsUnterminatedInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "open single quote", raw: `--regex 'abc`, wantErr: "unterminated quoted string"},
		{name: "open double quote", raw: `--regex "abc`, wantErr: "unterminated quoted string"},
		{name: "lone single quote", raw: `'`, wantErr: "unterminated quoted string"},
		{name: "escaped closing double quote", raw: `--regex "abc\"`, wantErr: "unterminated quoted string"},
		{name: "backslash at end of double quote", raw: `--regex "abc\`, wantErr: "unterminated quoted string"},
		{name: "trailing backslash", raw: `--regex abc\`, wantErr: "unterminated escape sequence"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitInteractiveArgs(tt.raw)
			if err == nil {
				t.Fatalf("splitInteractiveArgs(%q) = %q, want error %q", tt.raw, got, tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("splitInteractiveArgs(%q) error = %q, want %q", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestParseInteractiveCommandReloadRegexQuoting(t *testing.T) {
	current := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
		LContext: lcontext.LContext{MaxCount: 7},
	}

	tests := []struct {
		name      string
		line      string
		wantRegex string
		wantMax   int
	}{
		{name: "single-quoted backslash regex", line: `:reload --regex '\d+'`, wantRegex: `\d+`, wantMax: 7},
		{name: "double-quoted backslash regex", line: `:reload --regex "a\.b"`, wantRegex: `a\.b`, wantMax: 7},
		{name: "empty regex clears it", line: `:reload --regex ''`, wantRegex: "", wantMax: 7},
		{name: "empty regex keeps following flag", line: `:reload --regex '' --max 1`, wantRegex: "", wantMax: 1},
		// --grep follows the dgrep/dtail command lines: a non-empty --grep
		// wins whatever the order (an empty one is rejected, see
		// TestParseInteractiveCommandRejectsBlankValues).
		{name: "grep wins when given first", line: `:reload --grep WARN --regex INFO`, wantRegex: "WARN", wantMax: 7},
		{name: "grep wins when given last", line: `:reload --regex INFO --grep WARN`, wantRegex: "WARN", wantMax: 7},
		// Only the command is trimmed: an escaped trailing blank is kept.
		{name: "escaped trailing space", line: `:reload --regex foo\ `, wantRegex: "foo ", wantMax: 7},
		{name: "escaped trailing tab", line: ":reload --regex foo\\\t", wantRegex: "foo\t", wantMax: 7},
		{name: "escaped trailing space before line ending", line: ":reload --regex foo\\ \r\n", wantRegex: "foo ", wantMax: 7},
		{name: "leading blanks and trailing blanks", line: " \t:reload --regex foo  \t", wantRegex: "foo", wantMax: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, err := parseInteractiveCommand(current, tt.line, logging.NopLogger{})
			if err != nil {
				t.Fatalf("parseInteractiveCommand(%q) error = %v", tt.line, err)
			}
			if command.next.RegexStr != tt.wantRegex || command.spec.Regex != tt.wantRegex {
				t.Fatalf("regex = %q, spec regex = %q, want %q",
					command.next.RegexStr, command.spec.Regex, tt.wantRegex)
			}
			if command.next.MaxCount != tt.wantMax {
				t.Fatalf("max = %d, want %d", command.next.MaxCount, tt.wantMax)
			}
		})
	}
}

func TestParseInteractiveCommandRejectsBlankValues(t *testing.T) {
	grep := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	cat := config.Args{Mode: omode.CatClient, What: "/var/log/app.log"}
	mapr := config.Args{
		Mode:     omode.MapClient,
		What:     "/var/log/app.log",
		QueryStr: "select count(status) from stats group by status",
	}
	tailQuery := mapr
	tailQuery.Mode = omode.TailClient

	tests := []struct {
		name    string
		current config.Args
		line    string
		wantErr string
	}{
		{name: "empty files", current: grep, line: `:reload --files ''`, wantErr: "--files needs at least one non-blank file"},
		{name: "blank files", current: cat, line: `:reload --files ' '`, wantErr: "--files needs at least one non-blank file"},
		{name: "comma files", current: cat, line: `:reload --files ,`, wantErr: "--files needs at least one non-blank file"},
		{name: "blank comma files", current: mapr, line: `:reload --files ' , '`, wantErr: "--files needs at least one non-blank file"},
		{name: "empty files after equals", current: grep, line: `:reload --files=`, wantErr: "--files needs at least one non-blank file"},
		{name: "empty dmap query", current: mapr, line: `:reload --query ''`, wantErr: "--query must not be blank"},
		{name: "blank dmap query", current: mapr, line: `:reload --query '  '`, wantErr: "--query must not be blank"},
		{name: "empty dtail query", current: tailQuery, line: `:reload --query ""`, wantErr: "--query must not be blank"},
		{name: "empty extra argument", current: grep, line: `:reload --plain ''`, wantErr: `unexpected arguments: ""`},
		{name: "extra arguments quoted", current: cat, line: `:reload --plain foo ' '`, wantErr: `unexpected arguments: "foo" " "`},
		{name: "empty grep", current: grep, line: `:reload --grep ""`, wantErr: "--grep '' is ignored; use --regex '' to clear the regex"},
		{name: "empty grep with regex", current: grep, line: `:reload --regex INFO --grep ''`, wantErr: "--grep '' is ignored"},
		{name: "empty grep with empty regex", current: grep, line: `:reload --grep '' --regex ''`, wantErr: "--grep '' is ignored"},
		{name: "empty grep after equals", current: grep, line: `:reload --grep=`, wantErr: "--grep '' is ignored"},
		{name: "invalid utf-8 regex", current: grep, line: ":reload --regex '\xff'", wantErr: "reload line is not valid UTF-8"},
		{name: "invalid utf-8 files", current: cat, line: ":reload --files /tmp/\xfe.log", wantErr: "reload line is not valid UTF-8"},
		{name: "blank reload", current: cat, line: ":reload \t ", wantErr: "reload requires flags to change"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, err := parseInteractiveCommand(tt.current, tt.line, logging.NopLogger{})
			if err == nil {
				t.Fatalf("parseInteractiveCommand(%q) = %#v, want error %q", tt.line, command.next, tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), tt.wantErr) {
				t.Fatalf("parseInteractiveCommand(%q) error = %q, want prefix %q", tt.line, err, tt.wantErr)
			}
		})
	}
}

func TestParseInteractiveCommandKeepsUnsetBlankFiles(t *testing.T) {
	// A session without files (e.g. serverless stdin) can still reload other
	// flags: only an explicitly given --files value is validated.
	current := config.Args{Mode: omode.GrepClient, RegexStr: "ERROR"}
	command, err := parseInteractiveCommand(current, `:reload --regex WARN`, logging.NopLogger{})
	if err != nil {
		t.Fatalf("parseInteractiveCommand() error = %v", err)
	}
	if command.next.RegexStr != "WARN" || command.next.What != "" {
		t.Fatalf("regex = %q, files = %q, want WARN and no files", command.next.RegexStr, command.next.What)
	}
	command, err = parseInteractiveCommand(current, `:reload --files 'a.log, ,b.log'`, logging.NopLogger{})
	if err != nil {
		t.Fatalf("parseInteractiveCommand() error = %v", err)
	}
	if !reflect.DeepEqual(command.spec.Files, []string{"a.log", "b.log"}) {
		t.Fatalf("spec files = %q, want [a.log b.log]", command.spec.Files)
	}
}

func TestInteractiveHelpTextMatchesMode(t *testing.T) {
	query := "select count(status) from stats group by status"
	tests := []struct {
		name        string
		args        config.Args
		wantExample string
	}{
		{name: "dgrep", args: config.Args{Mode: omode.GrepClient}, wantExample: `--regex '\d+'`},
		{name: "dtail", args: config.Args{Mode: omode.TailClient}, wantExample: `--regex '\d+'`},
		{name: "dtail query", args: config.Args{Mode: omode.TailClient, QueryStr: query}, wantExample: "--query"},
		{name: "dmap", args: config.Args{Mode: omode.MapClient, QueryStr: query}, wantExample: "--query"},
		{name: "dcat", args: config.Args{Mode: omode.CatClient}, wantExample: "--files /var/log/a.log,/var/log/b.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			help := interactiveHelpText(tt.args)
			if !strings.Contains(help, ":reload <flags>, :show, :help, :quit") {
				t.Fatalf("help %q lacks the command list", help)
			}
			if !strings.Contains(help, tt.wantExample) {
				t.Fatalf("help %q lacks example %q", help, tt.wantExample)
			}
			// A path with a space needs servers advertising
			// encoded-read-path-v1, so the generic examples avoid one.
			if strings.Contains(help, "my app") || strings.Contains(help, `\ `) {
				t.Fatalf("help %q suggests a file path with a space", help)
			}
			// Only the grep-driven modes accept --regex.
			grepMode := tt.wantExample == `--regex '\d+'`
			if strings.Contains(help, "--regex") != grepMode {
				t.Fatalf("help %q: mentions --regex = %v, want %v", help, !grepMode, grepMode)
			}
		})
	}
}

// TestParseInteractiveCommandToleratesSurroundingBlanks checks that the
// commands without flags are recognised with surrounding spaces, tabs and a
// trailing carriage return, as the control loop passes lines on untrimmed.
func TestParseInteractiveCommandToleratesSurroundingBlanks(t *testing.T) {
	current := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log"}
	for _, kind := range []string{"show", "help", "quit"} {
		for _, line := range []string{
			":" + kind,
			"  :" + kind + "  ",
			"\t:" + kind + "\t",
			":" + kind + "\r",
			" \t:" + kind + " \t\r",
		} {
			command, err := parseInteractiveCommand(current, line, logging.NopLogger{})
			if err != nil {
				t.Fatalf("parseInteractiveCommand(%q) error = %v", line, err)
			}
			if command.kind != kind {
				t.Fatalf("parseInteractiveCommand(%q) kind = %q, want %q", line, command.kind, kind)
			}
		}
	}
}

// TestParseInteractiveCommandRequiresExactReload checks that only ":reload"
// alone or followed by a space or tab is the reload command.
func TestParseInteractiveCommandRequiresExactReload(t *testing.T) {
	current := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}

	for _, line := range []string{":reload --regex WARN", ":reload\t--regex WARN", "\t:reload --regex WARN\r"} {
		command, err := parseInteractiveCommand(current, line, logging.NopLogger{})
		if err != nil {
			t.Fatalf("parseInteractiveCommand(%q) error = %v", line, err)
		}
		if command.kind != "reload" || command.next.RegexStr != "WARN" {
			t.Fatalf("parseInteractiveCommand(%q) = kind %q regex %q, want reload WARN",
				line, command.kind, command.next.RegexStr)
		}
	}

	for _, line := range []string{":reload", " :reload\r", ":reload \t"} {
		_, err := parseInteractiveCommand(current, line, logging.NopLogger{})
		if err == nil || !strings.Contains(err.Error(), "reload requires flags") {
			t.Fatalf("parseInteractiveCommand(%q) error = %v, want reload requires flags", line, err)
		}
	}

	for _, line := range []string{":reloadx", ":reload--regex=x", ":reloads --regex x", ":reload,--regex x"} {
		_, err := parseInteractiveCommand(current, line, logging.NopLogger{})
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("parseInteractiveCommand(%q) error = %v, want unknown command", line, err)
		}
	}
}

func TestApplyInteractiveReloadRejectsUnsupportedConnections(t *testing.T) {
	client := &baseClient{
		mu: newBaseClientMu(),
		Args: config.Args{
			Mode:     omode.GrepClient,
			What:     "/var/log/app.log",
			RegexStr: "ERROR",
		},
		sessionSpec: SessionSpec{
			Mode:  omode.GrepClient,
			Files: []string{"/var/log/app.log"},
			Regex: "ERROR",
		},
		connections: []connectors.Connector{
			&interactiveReloadConnector{server: "srv1", supported: false},
		},
	}

	err := client.applyInteractiveReload(config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/next.log",
		RegexStr: "WARN",
	}, SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/next.log"},
		Regex: "WARN",
	})
	if !errors.Is(err, connectors.ErrSessionUnsupported) {
		t.Fatalf("expected ErrSessionUnsupported, got %v", err)
	}
	if client.What != "/var/log/app.log" || client.sessionSpec.Regex != "ERROR" {
		t.Fatalf("client state changed on unsupported reload: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
}

func TestApplyInteractiveReloadRollsBackEarlyFailure(t *testing.T) {
	oldArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	rollbackErr := errors.New("boom")

	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connC := &interactiveReloadConnector{server: "srv3", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, applyErr: rollbackErr, generation: 4}
	recorder := &interactiveReloadRecorder{}

	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB, connC},
		profile: clientProfile{
			commit: recorder.commitSessionSpec,
		},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || !errors.Is(err, rollbackErr) {
		t.Fatalf("expected reload error from failing connection, got %v", err)
	}

	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed on partial failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if len(recorder.commits) != 0 {
		t.Fatalf("expected no committed shared state, got %#v", recorder.commits)
	}
	if !reflect.DeepEqual(connA.committedSpec, oldSpec) || !reflect.DeepEqual(connB.committedSpec, oldSpec) {
		t.Fatalf("expected successful connections to roll back to old spec: %#v %#v", connA.committedSpec, connB.committedSpec)
	}
	if connA.generation != 6 || connB.generation != 6 {
		t.Fatalf("expected rollback to advance generations through the remote update path, got %d and %d", connA.generation, connB.generation)
	}
	if connA.applyCount != 2 || connB.applyCount != 2 || connC.applyCount != 1 {
		t.Fatalf("unexpected apply counts during rollback: %#v %#v %#v", connA.applyCount, connB.applyCount, connC.applyCount)
	}
	if !reflect.DeepEqual(connA.liveSpec, oldSpec) || !reflect.DeepEqual(connB.liveSpec, oldSpec) {
		t.Fatalf("expected rollback to restore live specs on successful connections: %#v %#v", connA.liveSpec, connB.liveSpec)
	}
	if !reflect.DeepEqual(connC.committedSpec, oldSpec) || connC.generation != 4 {
		t.Fatalf("failed connection should remain on original committed session: %#v generation=%d", connC.committedSpec, connC.generation)
	}
}

func TestApplyInteractiveReloadRollsBackLateAckFailure(t *testing.T) {
	oldArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connC := &interactiveReloadConnector{
		server:                   "srv3",
		supported:                true,
		committedSpec:            oldSpec,
		liveSpec:                 oldSpec,
		generation:               4,
		applyErrAfterAdvance:     connectors.ErrSessionAckTimeout,
		applyErrAfterAdvanceSpec: nextSpec,
	}

	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB, connC},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || !errors.Is(err, connectors.ErrSessionAckTimeout) {
		t.Fatalf("expected late ack timeout error, got %v", err)
	}

	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed on late failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if !reflect.DeepEqual(connA.committedSpec, oldSpec) || !reflect.DeepEqual(connB.committedSpec, oldSpec) {
		t.Fatalf("expected successful connections to roll back to old spec: %#v %#v", connA.committedSpec, connB.committedSpec)
	}
	if connA.generation != 6 || connB.generation != 6 {
		t.Fatalf("expected rollback to advance generations through the remote update path, got %d and %d", connA.generation, connB.generation)
	}
	if connA.applyCount != 2 || connB.applyCount != 2 || connC.applyCount != 2 {
		t.Fatalf("unexpected apply counts during late rollback: %#v %#v %#v", connA.applyCount, connB.applyCount, connC.applyCount)
	}
	if !reflect.DeepEqual(connA.liveSpec, oldSpec) || !reflect.DeepEqual(connB.liveSpec, oldSpec) {
		t.Fatalf("expected rollback to restore live specs on successful connections: %#v %#v", connA.liveSpec, connB.liveSpec)
	}
	if !reflect.DeepEqual(connC.committedSpec, oldSpec) || !reflect.DeepEqual(connC.liveSpec, oldSpec) || connC.generation != 6 {
		t.Fatalf("late-failing connection should also roll back to original committed session: committed=%#v live=%#v generation=%d", connC.committedSpec, connC.liveSpec, connC.generation)
	}
}

func TestApplyInteractiveReloadRollsBackLateAckFailureWithRealServerConnection(t *testing.T) {
	oldArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	oldSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/tmp/new.log"},
		Regex: "WARN",
	}

	handlerA := newInteractiveReloadSessionHandler(
		handlers.SessionAck{Action: "update", Generation: 5},
		handlers.SessionAck{Action: "update", Generation: 6},
	)
	handlerB := newInteractiveReloadSessionHandler(
		handlers.SessionAck{Action: "update", Generation: 5},
		handlers.SessionAck{Action: "update", Generation: 6},
	)
	handlerC := newInteractiveReloadSessionHandler()
	connA := newInteractiveReloadServerConnection(t, "srv1", handlerA, oldSpec)
	connB := newInteractiveReloadServerConnection(t, "srv2", handlerB, oldSpec)
	connC := newInteractiveReloadServerConnection(t, "srv3", handlerC, oldSpec)

	for _, conn := range []*connectors.ServerConnection{connA, connB, connC} {
		conn.RestoreCommittedSession(oldSpec, 4, true)
	}

	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB, connC},
	}

	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/tmp/new.log",
		RegexStr: "WARN",
	}
	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if err == nil || !errors.Is(err, connectors.ErrSessionAckTimeout) {
		t.Fatalf("expected late ack timeout error, got %v", err)
	}

	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed on late failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}

	for _, conn := range []*connectors.ServerConnection{connA, connB} {
		committedSpec, generation, ok := conn.CommittedSession()
		if !ok || generation != 6 || !reflect.DeepEqual(committedSpec, oldSpec) {
			t.Fatalf("successful connection did not roll back cleanly: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
		}
	}

	if committedSpec, generation, ok := connC.CommittedSession(); !ok || generation != 4 || !reflect.DeepEqual(committedSpec, oldSpec) {
		t.Fatalf("timed-out rollback should leave failed connection at its original committed session: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
	if len(handlerC.commands) != 2 {
		t.Fatalf("expected the failed connection to receive both the reload and rollback commands, got %#v", handlerC.commands)
	}
}

// TestApplyInteractiveReloadChecksReadCapabilitiesUpFront covers a mixed
// fleet: server srv1 reads encoded (spaced) paths, srv2 supports reloads but
// not encoded paths. A :reload to a spaced path is rejected before any
// SESSION UPDATE is sent, naming srv2, so srv1 neither switches nor has to be
// rolled back (which would repeat the previous workload's output), and
// nothing is reported to the user, so the exit status stays 0.
func TestApplyInteractiveReloadChecksReadCapabilitiesUpFront(t *testing.T) {
	oldArgs := config.Args{Mode: omode.TailClient, What: "/var/log/app.log"}
	oldSpec := SessionSpec{Mode: omode.TailClient, Files: []string{"/var/log/app.log"}}
	nextArgs := config.Args{Mode: omode.TailClient, What: "/x/my app.log"}
	nextSpec := SessionSpec{Mode: omode.TailClient, Files: []string{"/x/my app.log"}}

	handlerA := newInteractiveReloadSessionHandler(handlers.SessionAck{Action: "update", Generation: 5})
	handlerA.capabilities[protocol.CapabilityEncodedReadPathV1] = true
	handlerB := newInteractiveReloadSessionHandler(handlers.SessionAck{Action: "update", Generation: 5})
	connA := newInteractiveReloadServerConnection(t, "srv1", handlerA, oldSpec)
	connB := newInteractiveReloadServerConnection(t, "srv2", handlerB, oldSpec)
	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB},
	}

	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if !errors.Is(err, connectors.ErrEncodedReadPathUnsupported) {
		t.Fatalf("expected ErrEncodedReadPathUnsupported, got %v", err)
	}
	if !strings.HasSuffix(err.Error(), ": srv2") {
		t.Fatalf("error %q should name srv2 only", err)
	}
	for _, handler := range []*interactiveReloadSessionHandler{handlerA, handlerB} {
		if len(handler.commands) != 0 || len(handler.reported) != 0 {
			t.Fatalf("rejected reload sent %#v and reported %#v", handler.commands, handler.reported)
		}
	}
	for _, conn := range []*connectors.ServerConnection{connA, connB} {
		if spec, generation, ok := conn.CommittedSession(); !ok || generation != 4 || !reflect.DeepEqual(spec, oldSpec) {
			t.Fatalf("%s committed session changed: spec=%#v generation=%d ok=%v", conn.Server(), spec, generation, ok)
		}
	}
	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}

	// Once srv2 can read encoded paths too, the same reload applies on both.
	handlerB.capabilities[protocol.CapabilityEncodedReadPathV1] = true
	if err := client.applyInteractiveReload(nextArgs, nextSpec); err != nil {
		t.Fatalf("reload with capable servers: %v", err)
	}
	for _, conn := range []*connectors.ServerConnection{connA, connB} {
		if spec, generation, ok := conn.CommittedSession(); !ok || generation != 5 || !reflect.DeepEqual(spec, nextSpec) {
			t.Fatalf("%s not switched: spec=%#v generation=%d ok=%v", conn.Server(), spec, generation, ok)
		}
	}
}

func TestApplyInteractiveReloadCommitsSharedState(t *testing.T) {
	oldSpec := SessionSpec{
		Mode:  omode.MapClient,
		Files: []string{"/var/log/app.log"},
		Query: "select count(status) from stats group by status",
		Regex: "\\|MAPREDUCE:STATS\\|",
	}
	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	recorder := &interactiveReloadRecorder{}

	client := &baseClient{
		mu: newBaseClientMu(),
		Args: config.Args{
			Mode:     omode.MapClient,
			What:     "/var/log/app.log",
			QueryStr: "select count(status) from stats group by status",
		},
		sessionSpec: SessionSpec{
			Mode:  omode.MapClient,
			Files: []string{"/var/log/app.log"},
			Query: "select count(status) from stats group by status",
			Regex: "\\|MAPREDUCE:STATS\\|",
		},
		connections: []connectors.Connector{connA, connB},
		profile: clientProfile{
			commit: recorder.commitSessionSpec,
		},
	}

	nextArgs := config.Args{
		Mode:     omode.MapClient,
		What:     "/tmp/new.log",
		QueryStr: "select count(status) from warnings group by status",
		Plain:    true,
		Timeout:  5,
	}
	nextSpec := SessionSpec{
		Mode:    omode.MapClient,
		Files:   []string{"/tmp/new.log"},
		Query:   nextArgs.QueryStr,
		Regex:   "\\|MAPREDUCE:WARNINGS\\|",
		Timeout: 5,
	}

	if err := client.applyInteractiveReload(nextArgs, nextSpec); err != nil {
		t.Fatalf("applyInteractiveReload() error = %v", err)
	}

	if client.What != "/tmp/new.log" || client.sessionSpec.Query != nextArgs.QueryStr {
		t.Fatalf("client state not committed: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	if len(recorder.commits) != 1 {
		t.Fatalf("expected one profile commit call, got %d", len(recorder.commits))
	}
	if recorder.commits[0].generation != 5 || recorder.commits[0].spec.Query != nextArgs.QueryStr {
		t.Fatalf("unexpected commit payload: %#v", recorder.commits[0])
	}
	if connA.committedSpec.Query != nextArgs.QueryStr || connB.committedSpec.Query != nextArgs.QueryStr {
		t.Fatalf("connectors did not receive new session spec: %#v %#v", connA.committedSpec, connB.committedSpec)
	}
}

func TestApplyInteractiveReloadRollsBackProfileCommitError(t *testing.T) {
	oldArgs := config.Args{Mode: omode.MapClient, What: "/var/log/app.log"}
	oldSpec := SessionSpec{Mode: omode.MapClient, Files: []string{"/var/log/app.log"}, Query: "from STATS select count(*)"}
	nextArgs := config.Args{Mode: omode.MapClient, What: "/tmp/new.log"}
	nextSpec := SessionSpec{Mode: omode.MapClient, Files: []string{"/tmp/new.log"}, Query: "from WARNINGS select count(*)"}
	connA := &interactiveReloadConnector{server: "srv1", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	connB := &interactiveReloadConnector{server: "srv2", supported: true, committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	commitErr := errors.New("commit failed")
	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB},
		profile: clientProfile{
			commit: func(SessionSpec, uint64) error { return commitErr },
		},
	}

	err := client.applyInteractiveReload(nextArgs, nextSpec)
	if !errors.Is(err, commitErr) {
		t.Fatalf("applyInteractiveReload() error = %v, want commit error", err)
	}
	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed after commit failure: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	for _, conn := range []*interactiveReloadConnector{connA, connB} {
		if !reflect.DeepEqual(conn.committedSpec, oldSpec) || !reflect.DeepEqual(conn.liveSpec, oldSpec) {
			t.Fatalf("connection %s did not roll back: committed=%#v live=%#v", conn.server, conn.committedSpec, conn.liveSpec)
		}
		if conn.generation != 6 || conn.applyCount != 2 {
			t.Fatalf("connection %s rollback state = generation:%d applies:%d, want 6 and 2", conn.server, conn.generation, conn.applyCount)
		}
	}
}

type interactiveReloadConnector struct {
	committedSpec            sessionspec.Spec
	liveSpec                 sessionspec.Spec
	applyErr                 error
	applyErrAfterAdvance     error
	applyErrAfterAdvanceSpec sessionspec.Spec
	applyCount               int
	generation               uint64
	server                   string
	supported                bool
}

type interactiveReloadSessionHandler struct {
	commands            []string
	reported            []string
	capabilities        map[string]bool
	sessionAcks         []handlers.SessionAck
	waitForCapabilities bool
}

var _ handlers.Handler = (*interactiveReloadSessionHandler)(nil)

func newInteractiveReloadSessionHandler(acks ...handlers.SessionAck) *interactiveReloadSessionHandler {
	return &interactiveReloadSessionHandler{
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
		sessionAcks:         append([]handlers.SessionAck(nil), acks...),
		waitForCapabilities: true,
	}
}

func (h *interactiveReloadSessionHandler) SendMessage(command string) error {
	h.commands = append(h.commands, command)
	return nil
}

func (h *interactiveReloadSessionHandler) Capabilities() []string {
	capabilities := make([]string, 0, len(h.capabilities))
	for capability := range h.capabilities {
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func (h *interactiveReloadSessionHandler) HasCapability(name string) bool {
	return h.capabilities[name]
}

func (h *interactiveReloadSessionHandler) ReportServerError(message string) {
	h.reported = append(h.reported, message)
}

func (*interactiveReloadSessionHandler) Server() string { return "mock" }

func (*interactiveReloadSessionHandler) Status() int { return 0 }

func (*interactiveReloadSessionHandler) Shutdown() {}

func (*interactiveReloadSessionHandler) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (h *interactiveReloadSessionHandler) WaitForCapabilities(time.Duration) bool {
	return h.waitForCapabilities
}

func (h *interactiveReloadSessionHandler) WaitForSessionAck(timeout time.Duration) (handlers.SessionAck, bool) {
	if timeout <= 0 {
		return handlers.SessionAck{}, false
	}
	if len(h.sessionAcks) == 0 {
		return handlers.SessionAck{}, false
	}

	ack := h.sessionAcks[0]
	h.sessionAcks = h.sessionAcks[1:]
	return ack, true
}

func (*interactiveReloadSessionHandler) Read(_ []byte) (int, error) {
	return 0, nil
}

func (*interactiveReloadSessionHandler) Write(p []byte) (int, error) {
	return len(p), nil
}

type interactiveReloadHostKeyCallback struct{}

func (interactiveReloadHostKeyCallback) Wrap(context.Context) ssh.HostKeyCallback {
	return ssh.InsecureIgnoreHostKey()
}

func (interactiveReloadHostKeyCallback) Untrusted(string) bool { return false }

func (interactiveReloadHostKeyCallback) PromptAddHosts(context.Context) {}

func newInteractiveReloadServerConnection(t *testing.T, server string, handler handlers.Handler, spec SessionSpec) *connectors.ServerConnection {
	t.Helper()

	conn, err := connectors.NewServerConnection(
		server,
		"user",
		nil,
		interactiveReloadHostKeyCallback{},
		handler,
		nil,
		spec,
		true,
		"",
		false,
		nil,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("NewServerConnection: %v", err)
	}
	conn.RestoreCommittedSession(spec, 4, true)
	return conn
}

func (*interactiveReloadConnector) Start(context.Context, context.CancelFunc, chan struct{}, chan struct{}) {
}

func (c *interactiveReloadConnector) Server() string { return c.server }

func (*interactiveReloadConnector) Handler() handlers.Handler { return nil }

func (c *interactiveReloadConnector) SupportsQueryUpdates(time.Duration) bool { return c.supported }

func (c *interactiveReloadConnector) ApplySessionSpec(spec sessionspec.Spec, _ time.Duration) error {
	_, generation, ok := c.CommittedSession()
	if !ok {
		generation = 0
	}
	return c.ApplySessionSpecWithGeneration(spec, generation, 0)
}

func (c *interactiveReloadConnector) ApplySessionSpecWithGeneration(spec sessionspec.Spec, generation uint64, _ time.Duration) error {
	c.applyCount++
	if c.applyErrAfterAdvance != nil && reflect.DeepEqual(spec, c.applyErrAfterAdvanceSpec) {
		c.liveSpec = spec
		return c.applyErrAfterAdvance
	}
	if c.applyErr != nil {
		return c.applyErr
	}
	if generation == 0 {
		c.generation = 1
	} else {
		c.generation = generation + 1
	}
	c.liveSpec = spec
	c.committedSpec = spec
	return nil
}

func (c *interactiveReloadConnector) CommittedSession() (sessionspec.Spec, uint64, bool) {
	if c.generation == 0 {
		return sessionspec.Spec{}, 0, false
	}
	return c.committedSpec, c.generation, true
}

func (c *interactiveReloadConnector) RestoreCommittedSession(spec sessionspec.Spec, generation uint64, committed bool) {
	c.liveSpec = spec
	c.committedSpec = spec
	c.generation = generation
}

type interactiveReloadRecorder struct {
	commits []interactiveReloadCommit
}

type interactiveReloadCommit struct {
	generation uint64
	spec       SessionSpec
}

func (r *interactiveReloadRecorder) commitSessionSpec(spec SessionSpec, generation uint64) error { //nolint:unparam // The profile commit callback permits commit errors.
	r.commits = append(r.commits, interactiveReloadCommit{
		generation: generation,
		spec:       spec,
	})
	return nil
}

// TestRunInteractiveCommand covers the dispatch of one parsed control
// command: every kind writes its answer line to the TTY, :quit reports quit,
// a failed reload is answered on the TTY without an error, and only a failed
// TTY write is returned as an error (which ends the control loop).
func TestRunInteractiveCommand(t *testing.T) {
	spec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"}
	args := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	nextSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: "WARN"}
	nextArgs := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "WARN"}
	writeErr := errors.New("tty gone")

	tests := []struct {
		name        string
		command     interactiveCommand
		connections func() []connectors.Connector
		failWrite   bool
		wantQuit    bool
		wantOutput  string // exact output; empty when wantPrefix is used
		wantPrefix  string
		wantErr     error
		wantRegex   string // client regex after the command
	}{
		{name: "quit", command: interactiveCommand{kind: "quit"},
			wantQuit: true, wantOutput: "quitting interactive session\n", wantRegex: "ERROR"},
		{name: "reload failure is answered, not returned",
			command:    interactiveCommand{kind: "reload", next: nextArgs, spec: nextSpec},
			wantOutput: "reload failed: no active connections\n", wantRegex: "ERROR"},
		{name: "reload success",
			command: interactiveCommand{kind: "reload", next: nextArgs, spec: nextSpec},
			connections: func() []connectors.Connector {
				return []connectors.Connector{&interactiveReloadConnector{
					server: "srv1", supported: true, committedSpec: spec, liveSpec: spec, generation: 1}}
			},
			wantOutput: "reload applied successfully\n", wantRegex: "WARN"},
		{name: "show", command: interactiveCommand{kind: "show"},
			wantPrefix: "mode=", wantRegex: "ERROR"},
		{name: "help", command: interactiveCommand{kind: "help"},
			wantOutput: interactiveHelpText(args) + "\n", wantRegex: "ERROR"},
		{name: "unsupported", command: interactiveCommand{kind: "bogus"},
			wantOutput: "unsupported command\n", wantRegex: "ERROR"},
		{name: "quit write error", command: interactiveCommand{kind: "quit"},
			failWrite: true, wantQuit: true, wantErr: writeErr, wantRegex: "ERROR"},
		{name: "reload failure write error",
			command:   interactiveCommand{kind: "reload", next: nextArgs, spec: nextSpec},
			failWrite: true, wantErr: writeErr, wantRegex: "ERROR"},
		{name: "help write error", command: interactiveCommand{kind: "help"},
			failWrite: true, wantErr: writeErr, wantRegex: "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &baseClient{mu: newBaseClientMu(), Args: args, sessionSpec: spec}
			if tt.connections != nil {
				client.connections = tt.connections()
			}
			var out bytes.Buffer
			var tty io.Writer = &out
			if tt.failWrite {
				tty = failedPayloadWriter{err: writeErr}
			}

			quit, err := client.runInteractiveCommand(tty, tt.command)
			if quit != tt.wantQuit {
				t.Fatalf("quit = %v, want %v", quit, tt.wantQuit)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantOutput != "" && out.String() != tt.wantOutput {
				t.Fatalf("output = %q, want %q", out.String(), tt.wantOutput)
			}
			if tt.wantPrefix != "" && !strings.HasPrefix(out.String(), tt.wantPrefix) {
				t.Fatalf("output = %q, want prefix %q", out.String(), tt.wantPrefix)
			}
			if got, _ := client.snapshotConnectionState(); got.RegexStr != tt.wantRegex {
				t.Fatalf("client regex = %q, want %q", got.RegexStr, tt.wantRegex)
			}
		})
	}
}
