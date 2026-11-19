package clients

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// TestParseQueryAndTailCommands parses :query and :tail lines: the rest of
// the line is the query or regex as typed (no shell quoting), only the
// blanks around it are dropped; :query alone shows the query; the commands
// exist only in the modes that can run them, and an invalid query or regex
// is rejected before anything runs.
func TestParseQueryAndTailCommands(t *testing.T) {
	tail := config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	tailQuery := config.Args{Mode: omode.TailClient, What: "/var/log/app.log", QueryStr: switchTestQuery}
	dmap := config.Args{Mode: omode.MapClient, What: "/var/log/app.log", QueryStr: switchTestQuery}
	grep := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	cat := config.Args{Mode: omode.CatClient, What: "/var/log/app.log"}
	quoted := `from STATS select count($line) where $hostname eq "host 1" group by $hostname`

	tests := []struct {
		name      string
		current   config.Args
		line      string
		wantKind  string
		wantValue string
		wantErr   string
	}{
		{name: "query", current: tail, line: ":query " + switchTestQuery, wantKind: "query", wantValue: switchTestQuery},
		{name: "query keeps its quotes", current: tail, line: ":query  " + quoted + " \r", wantKind: "query",
			wantValue: quoted},
		{name: "query with a tab", current: tailQuery, line: "\t:query\t" + switchTestQuery, wantKind: "query",
			wantValue: switchTestQuery},
		{name: "dmap query", current: dmap, line: ":query " + switchTestQuery, wantKind: "query", wantValue: switchTestQuery},
		{name: "show query", current: tailQuery, line: " :query  ", wantKind: "showquery"},
		{name: "show query while tailing", current: tail, line: ":query", wantKind: "showquery"},
		{name: "tail", current: tailQuery, line: ":tail", wantKind: "tail"},
		{name: "tail regex", current: tailQuery, line: `:tail  WARN|\d+ ms `, wantKind: "tail", wantValue: `WARN|\d+ ms`},
		{name: "tail regex keeps quotes", current: tail, line: `:tail 'a b'`, wantKind: "tail", wantValue: `'a b'`},
		{name: "invalid query", current: tail, line: ":query from STATS select", wantErr: "invalid query"},
		{name: "invalid regex", current: tailQuery, line: ":tail (", wantErr: "invalid regex"},
		{name: "invalid utf-8 query", current: tail, line: ":query from STATS select count(\xff)", wantErr: "not valid UTF-8"},
		{name: "invalid utf-8 regex", current: tail, line: ":tail \xfe", wantErr: "not valid UTF-8"},
		{name: "glued query", current: tail, line: ":queryfrom STATS", wantErr: "unknown command"},
		{name: "glued tail", current: tail, line: ":tailWARN", wantErr: "unknown command"},
		{name: "tail in dmap", current: dmap, line: ":tail", wantErr: ":tail is only available in dtail"},
		{name: "tail in dgrep", current: grep, line: ":tail WARN", wantErr: ":tail is only available in dtail"},
		{name: "query in dgrep", current: grep, line: ":query " + switchTestQuery, wantErr: ":query is only available in dtail and dmap"},
		{name: "query in dcat", current: cat, line: ":query", wantErr: ":query is only available in dtail and dmap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, err := parseInteractiveCommand(tt.current, tt.line)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseInteractiveCommand(%q) = %#v, %v; want error %q", tt.line, command, err, tt.wantErr)
				}
				return
			}
			if err != nil || command.kind != tt.wantKind || command.value != tt.wantValue {
				t.Fatalf("parseInteractiveCommand(%q) = kind %q value %q, err %v; want %q %q",
					tt.line, command.kind, command.value, err, tt.wantKind, tt.wantValue)
			}
		})
	}
}

// runControlLine parses line for the current arguments of c and runs it as
// the control loop does, returning what it wrote to the TTY.
func runControlLine(t *testing.T, c *baseClient, line string) string {
	t.Helper()
	args, _ := c.snapshotConnectionState()
	command, err := parseInteractiveCommand(args, line)
	if err != nil {
		t.Fatalf("parseInteractiveCommand(%q): %v", line, err)
	}
	var tty bytes.Buffer
	if _, err := c.runInteractiveCommand(&tty, command); err != nil {
		t.Fatalf("runInteractiveCommand(%q): %v", line, err)
	}
	return tty.String()
}

// TestQueryAndTailCommandsSwitchTheSession runs :query and :tail on the
// switchable client of interactive dtail with fake connections: :query
// switches every connection to the query at the next generation, :query
// alone shows it, a second :query replaces it, :tail goes back to the lines
// followed before and :tail with a regex follows other lines; :show names
// the workload each time.
func TestQueryAndTailCommandsSwitchTheSession(t *testing.T) {
	c, conns := newSwitchTestClient(t, config.Args{What: "/var/log/app.log", RegexStr: "ERROR"})
	base := &c.baseClient
	requireWorkload := func(want string, generation uint64, query, regex string) {
		t.Helper()
		for _, conn := range conns {
			if conn.committedSpec.Query != query || conn.committedSpec.Regex != regex || conn.generation != generation {
				t.Fatalf("%s spec = %#v at %d, want query %q regex %q at %d", conn.server, conn.committedSpec,
					conn.generation, query, regex, generation)
			}
		}
		if show := runControlLine(t, base, ":show"); !strings.Contains(show, " workload="+want+" ") {
			t.Fatalf(":show = %q, want workload=%s", show, want)
		}
	}

	if out := runControlLine(t, base, ":query"); !strings.Contains(out, "no query runs") {
		t.Fatalf(":query while tailing = %q", out)
	}
	out := runControlLine(t, base, ":query "+switchTestQuery)
	if !strings.Contains(out, "query applied") || !strings.Contains(out, "query: "+switchTestQuery) {
		t.Fatalf(":query = %q", out)
	}
	requireWorkload("query", 5, switchTestQuery, `\|MAPREDUCE:STATS\|`)
	if out := runControlLine(t, base, ":query"); out != "query: "+switchTestQuery+"\n" {
		t.Fatalf(":query alone = %q", out)
	}
	other := "from STATS select max($goroutines) group by $hostname"
	runControlLine(t, base, ":query "+other)
	requireWorkload("query", 6, other, `\|MAPREDUCE:STATS\|`)

	if out := runControlLine(t, base, ":tail"); !strings.Contains(out, "following lines") ||
		!strings.Contains(out, `regex: "ERROR"`) {
		t.Fatalf(":tail = %q", out)
	}
	requireWorkload("tail", 7, "", "ERROR")
	runControlLine(t, base, ":query "+switchTestQuery)
	requireWorkload("query", 8, switchTestQuery, `\|MAPREDUCE:STATS\|`)
	runControlLine(t, base, ":tail WARN")
	requireWorkload("tail", 9, "", "WARN")
}

// TestQueryAndTailCommandsFailWithoutChange runs switches that can not be
// applied: :tail while the session already follows lines, and a :query
// whose update fails on a server. Each is answered on the TTY and leaves
// the running workload as it was (the server updated before the failing
// one is rolled back with another update, so its generation advances).
func TestQueryAndTailCommandsFailWithoutChange(t *testing.T) {
	c, conns := newSwitchTestClient(t, config.Args{What: "/var/log/app.log", RegexStr: "ERROR"})
	base := &c.baseClient
	if out := runControlLine(t, base, ":tail"); !strings.Contains(out, "tail failed: "+errAlreadyTailing.Error()) {
		t.Fatalf(":tail while tailing = %q", out)
	}
	conns[1].applyErr = connectors.ErrSessionRejected
	if out := runControlLine(t, base, ":query "+switchTestQuery); !strings.Contains(out, "query failed: ") {
		t.Fatalf(":query with a failing server = %q", out)
	}
	for _, conn := range conns {
		if conn.committedSpec.Query != "" || conn.committedSpec.Regex != "ERROR" {
			t.Fatalf("%s spec after failures = %#v at %d", conn.server, conn.committedSpec, conn.generation)
		}
	}
	if args, _ := c.snapshotConnectionState(); args.QueryStr != "" || args.RegexStr != "ERROR" {
		t.Fatalf("args after failures = %#v", args)
	}
}

// TestQueryCommandReplacesDmapQuery runs :query in dmap, which always runs
// a query: it replaces the query like :reload --query does, while :tail
// does not parse there.
func TestQueryCommandReplacesDmapQuery(t *testing.T) {
	args := config.Args{Mode: omode.MapClient, What: "/var/log/app.log", QueryStr: switchTestQuery,
		InteractiveQuery: true}
	_, spec, err := buildInteractiveSessionSpec(args)
	if err != nil {
		t.Fatal(err)
	}
	conn := &interactiveReloadConnector{server: "srv1", committedSpec: spec, liveSpec: spec, generation: 1}
	client := &baseClient{mu: newBaseClientMu(), Args: args, sessionSpec: spec,
		connections: []connectors.Connector{conn}}
	other := "from STATS select max($goroutines) group by $hostname"
	if out := runControlLine(t, client, ":query "+other); !strings.Contains(out, "query applied") {
		t.Fatalf(":query in dmap = %q", out)
	}
	if conn.committedSpec.Query != other || conn.generation != 2 {
		t.Fatalf("dmap spec = %#v at %d", conn.committedSpec, conn.generation)
	}
	if out := runControlLine(t, client, ":query"); out != "query: "+other+"\n" {
		t.Fatalf(":query alone in dmap = %q", out)
	}
}

// TestCompleteQueryCommand completes words of a :query line with the query
// completer, without shell quoting: keywords, the table and fields of the
// catalog, and a value in the query's own double quotes. The commands are
// offered on the first word only in the modes that have them.
func TestCompleteQueryCommand(t *testing.T) {
	tail := config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	grep := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log"}
	dmap := config.Args{Mode: omode.MapClient, What: "/var/log/app.log", QueryStr: switchTestQuery}
	catalog := catalogOf(queryCatalog)
	tests := []struct {
		name, line, want string
		args             config.Args
	}{
		{name: "keyword", args: tail, line: ":query fr", want: ":query from "},
		{name: "table", args: tail, line: ":query from ST", want: ":query from STATS "},
		{name: "select", args: tail, line: ":query from STATS sel", want: ":query from STATS select "},
		{name: "aggregation field", args: tail, line: ":query from STATS select count(gor",
			want: ":query from STATS select count(goroutines)"},
		{name: "dmap", args: dmap, line: ":query from STATS select count(gor",
			want: ":query from STATS select count(goroutines)"},
		{name: "value in the query's quotes", args: tail,
			line: `:query from STATS select count(goroutines) where $hostname eq "it`,
			want: `:query from STATS select count(goroutines) where $hostname eq "it's" `},
		{name: "commands in dtail", args: tail, line: ":q", want: ":qu"},
		{name: "tail command", args: tail, line: ":t", want: ":tail "},
		{name: "query command in dmap", args: dmap, line: ":qu", want: ":qu"},
		{name: "no query command in dgrep", args: grep, line: ":qu", want: ":quit"},
		{name: "no query completion in dgrep", args: grep, line: ":query fr", want: ":query fr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := completeInteractive(tt.line, len(tt.line), tt.args, catalog)
			if got.line != tt.want {
				t.Fatalf("completeInteractive(%q) = %q (candidates %q), want %q", tt.line, got.line,
					got.candidates, tt.want)
			}
		})
	}
	listed := completeInteractive(":qu", 3, tail, catalog)
	if slices.Sort(listed.candidates); strings.Join(listed.candidates, ",") != ":query ,:quit" {
		t.Fatalf("candidates of :qu in dtail = %q", listed.candidates)
	}
}

// TestHelpNamesQueryAndTail checks that :help names :query and :tail with
// an example in dtail, :query in dmap and neither in dgrep and dcat.
func TestHelpNamesQueryAndTail(t *testing.T) {
	for _, tt := range []struct {
		args              config.Args
		wantQuery, wantTl bool
	}{
		{args: config.Args{Mode: omode.TailClient, RegexStr: "x"}, wantQuery: true, wantTl: true},
		{args: config.Args{Mode: omode.TailClient, QueryStr: switchTestQuery}, wantQuery: true, wantTl: true},
		{args: config.Args{Mode: omode.MapClient, QueryStr: switchTestQuery}, wantQuery: true},
		{args: config.Args{Mode: omode.GrepClient}},
		{args: config.Args{Mode: omode.CatClient}},
	} {
		help := interactiveHelpText(tt.args)
		commands, _, _ := strings.Cut(help, "\n")
		if strings.Contains(commands, ":query") != tt.wantQuery || strings.Contains(commands, ":tail") != tt.wantTl {
			t.Fatalf("mode %s query %q: commands %q", tt.args.Mode, tt.args.QueryStr, commands)
		}
		if tt.wantQuery && !strings.Contains(help, "\n  :query ") {
			t.Fatalf("mode %s: no :query example in %q", tt.args.Mode, help)
		}
	}
}

// TestQueryAndTailLinesKeepAnExplicitPause runs rejected :query and :tail
// lines through handleInteractiveLine while the output is explicitly
// paused: each is answered with its error and the output stays paused,
// also :query and :tail in dgrep and dcat, which have neither.
func TestQueryAndTailLinesKeepAnExplicitPause(t *testing.T) {
	tail := config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	grep := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	cat := config.Args{Mode: omode.CatClient, What: "/var/log/app.log"}
	for _, tt := range []struct {
		name string
		args config.Args
		line string
	}{
		{name: "invalid query", args: tail, line: ":query from STATS select"},
		{name: "invalid regex", args: tail, line: ":tail ("},
		{name: "invalid utf-8", args: tail, line: ":query \xff"},
		{name: "query in dgrep", args: grep, line: ":query " + switchTestQuery},
		{name: "show query in dcat", args: cat, line: ":query"},
		{name: "tail in dgrep", args: grep, line: ":tail WARN"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logger := newPausingTestLogger()
			client := newPauseTestClient(logger)
			client.Args = tt.args
			pause := client.newInteractivePause()
			var tty bytes.Buffer
			if _, err := client.handleInteractiveLine(&tty, "", pause); err != nil {
				t.Fatalf("pause: %v", err)
			}
			tty.Reset()
			if quit, err := client.handleInteractiveLine(&tty, tt.line, pause); quit || err != nil {
				t.Fatalf("%q: quit=%v err=%v", tt.line, quit, err)
			}
			got := tty.String()
			if !strings.HasPrefix(got, "interactive query error: ") || strings.Contains(got, interactiveResumedMessage) ||
				logger.pauseDepth() != 1 {
				t.Fatalf("%q: tty=%q depth=%d, want the rejection with the output paused", tt.line, got,
					logger.pauseDepth())
			}
		})
	}
}

// TestQueryAndTailLinesResumeAnExplicitPause runs applied :query and :tail
// lines through handleInteractiveLine while the output is explicitly
// paused: each resumes the output before it runs (the servers'
// acknowledgements are read by the handlers the pause blocks) and is
// answered as applied, while :query alone only shows the query and keeps
// the pause.
func TestQueryAndTailLinesResumeAnExplicitPause(t *testing.T) {
	c, conns := newSwitchTestClient(t, config.Args{What: "/var/log/app.log", RegexStr: "ERROR"})
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	c.runtime.outputGate = newOutputGate()
	pause := c.newInteractivePause()
	for _, step := range []struct {
		line, want string
		resumes    bool
	}{
		{line: ":query " + switchTestQuery, want: "query applied", resumes: true},
		{line: ":query", want: "query: " + switchTestQuery},
		{line: ":tail", want: "following lines", resumes: true},
	} {
		var tty bytes.Buffer
		if _, err := c.handleInteractiveLine(&tty, "", pause); err != nil {
			t.Fatalf("pause: %v", err)
		}
		tty.Reset()
		if quit, err := c.handleInteractiveLine(&tty, step.line, pause); quit || err != nil {
			t.Fatalf("%q: quit=%v err=%v", step.line, quit, err)
		}
		got := tty.String()
		resumed := strings.HasPrefix(got, interactiveResumedMessage+"\n")
		if !strings.Contains(got, step.want) || resumed != step.resumes || (logger.pauseDepth() == 0) != step.resumes {
			t.Fatalf("%q: tty=%q depth=%d, want %q and resumed=%v", step.line, got, logger.pauseDepth(),
				step.want, step.resumes)
		}
		if !step.resumes {
			// Resume for the next step's explicit pause.
			if _, err := c.handleInteractiveLine(&tty, "", pause); err != nil {
				t.Fatalf("resume: %v", err)
			}
		}
	}
	if conns[0].generation != 6 {
		t.Fatalf("generation = %d, want 6 after two switches", conns[0].generation)
	}
}

// TestFailureMessagesEscapeServerText: the reason a server gives for a
// rejected update reaches the control TTY after "reload failed:", "query
// failed:" and "tail failed:". Its control and format characters are
// written as Go escapes, so server text can not drive the terminal (ESC),
// return the cursor (CR) or reorder the line (U+202E). A line feed in it
// is escaped too, so it can not forge a line of control output ("reload
// applied successfully"): the message has one line per error, as many as
// with a harmless reason.
func TestFailureMessagesEscapeServerText(t *testing.T) {
	const hostile = "\x1b[2Jevil\r\u202e\u009b\tend\nreload applied successfully"
	c, conns := newSwitchTestClient(t, config.Args{What: "/var/log/app.log", RegexStr: "ERROR"})
	base := &c.baseClient
	for _, conn := range conns {
		conn.applyErr = fmt.Errorf("%w: %s", connectors.ErrSessionRejected, hostile)
	}
	lines := map[string]string{
		"reload failed: ": ":reload --regex WARN",
		"query failed: ":  ":query " + switchTestQuery,
		"tail failed: ":   ":tail WARN",
	}
	for prefix, line := range lines {
		for _, conn := range conns {
			conn.applyErr = fmt.Errorf("%w: harmless", connectors.ErrSessionRejected)
		}
		harmless := runControlLine(t, base, line)
		for _, conn := range conns {
			conn.applyErr = fmt.Errorf("%w: %s", connectors.ErrSessionRejected, hostile)
		}
		out := runControlLine(t, base, line)
		if got, want := strings.Count(out, "\n"), strings.Count(harmless, "\n"); got != want {
			t.Errorf("%s = %q: %d lines, want the %d of a harmless reason (%q)", line, out, got, want, harmless)
		}
		if strings.Contains(out, "\nreload applied") || !strings.Contains(out, `end\nreload applied successfully`) {
			t.Errorf("%s = %q, want the reason's line feed escaped", line, out)
		}
		if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out, "\n") {
			t.Fatalf("%s = %q, want a %q line", line, out, prefix)
		}
		if strings.ContainsAny(out, "\x1b\r\t\u202e\u009b") {
			t.Errorf("%s wrote raw control or format characters: %q", line, out)
		}
		if !strings.Contains(out, `\x1b[2Jevil\r\u202e\u009b\tend`) {
			t.Errorf("%s = %q, want the server text escaped", line, out)
		}
	}
}

// TestTerminalTextKeepsPrintableText: text without control or format
// characters is unchanged byte for byte (quotes, backslashes, non-ASCII
// text and the line feeds of a joined error included); an invalid byte is
// written as \xNN.
func TestTerminalTextKeepsPrintableText(t *testing.T) {
	tests := map[string]string{
		"":                            "",
		"up: rejected \"a\\b\"\nlate": "up: rejected \"a\\b\"\nlate",
		"größe 日本 €":                  "größe 日本 €",
		"a\x00b\x7fc":                 `a\x00b\x7fc`,
		"line\u2028sep":               `line\u2028sep`,
		"bad\xffbyte\x80":             `bad\xffbyte\x80`,
	}
	for in, want := range tests {
		if got := terminalText(in); got != want {
			t.Errorf("terminalText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestErrorLines: the errors of an errors.Join, also a nested one, get a
// line each, and a line feed inside one of them is escaped; an error that
// is no join, also one wrapping two errors with a message of its own, is
// one line.
func TestErrorLines(t *testing.T) {
	first := errors.New("srv1: rejected: a\nb")
	second := errors.New("srv2: rollback")
	third := errors.New("srv3: rollback")
	tests := map[string]struct {
		err  error
		want string
	}{
		"single":      {first, `srv1: rejected: a\nb`},
		"join":        {errors.Join(first, second), `srv1: rejected: a\nb` + "\nsrv2: rollback"},
		"nested join": {errors.Join(first, errors.Join(second, third)), `srv1: rejected: a\nb` + "\nsrv2: rollback\nsrv3: rollback"},
		"wrapped join": {fmt.Errorf("reload: %w", errors.Join(second, third)),
			`reload: srv2: rollback\nsrv3: rollback`},
		"two wrapped": {fmt.Errorf("%w and %w", second, third), "srv2: rollback and srv3: rollback"},
	}
	for name, tt := range tests {
		if got := errorLines(tt.err); got != tt.want {
			t.Errorf("%s: errorLines = %q, want %q", name, got, tt.want)
		}
	}
}
