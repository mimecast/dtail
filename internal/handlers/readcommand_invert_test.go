package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
)

// TestReadCommandHonoursInvertedMatchAllPatterns runs a grep the two ways a
// command can be split into words before readCommand.Start: at every space,
// which keeps the empty last word of an empty pattern, and at runs of
// whitespace, which drops it. An inverted empty pattern must match no line
// either way rather than turn into a noop. A lone third word is a regex only
// with the "regex:" prefix: flags which do not parse fail the command, any
// other word is ignored and the command matches every line.
func TestReadCommandHonoursInvertedMatchAllPatterns(t *testing.T) {
	const content = "alpha\n\nbeta\n"
	sep := string([]byte{protocol.MessageDelimiter})
	all := "alpha\n" + sep + "\n" + sep + "beta\n" + sep

	tests := []struct {
		name        string
		regex       string
		split       func(string) []string
		want        string
		wantFailure bool
	}{
		{"split at spaces inverted empty", "regex:invert,literal ", splitSpaces, "", false},
		{"split at whitespace inverted empty", "regex:invert,literal ", strings.Fields, "", false},
		{"inverted match all", "regex:invert .*", strings.Fields, "", false},
		{"inverted dot keeps empty lines", "regex:invert .", strings.Fields, "\n" + sep, false},
		{"inverted literal", "regex:invert,literal alpha", strings.Fields, "\n" + sep + "beta\n" + sep, false},
		{"lone noop flags", "regex:noop ", strings.Fields, all, false},
		{"lone word which is no regex", ".", strings.Fields, all, false},
		{"lone word with regex prefix but no colon", "regexfoo", strings.Fields, all, false},
		{"lone unknown flag", "regex:bogus", strings.Fields, "", true},
		{"lone empty flag list", "regex:", strings.Fields, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSharedReadFile(t, content)
			server := newSharedReadTestServer(t, nil)
			cmd := newReadCommandWithDependencies(server.readCommandDependencies(), omode.GrepClient, nil)
			args := tt.split("grep " + path + " " + tt.regex)

			cmd.Start(context.Background(), lcontext.LContext{}, len(args), args, 1)

			if got := drained(t, server); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
			checkReadFailures(t, server.failureMessages(), tt.wantFailure)
		})
	}
}

// checkReadFailures checks that the command reported exactly one failure to
// parse the command when wantFailure is set, and none otherwise.
func checkReadFailures(t *testing.T, failures []string, wantFailure bool) {
	t.Helper()
	if !wantFailure {
		if len(failures) != 0 {
			t.Errorf("failure messages = %q, want none", failures)
		}
		return
	}
	if len(failures) != 1 || !strings.HasSuffix(failures[0], protocol.CommandFailureReadCommand) {
		t.Errorf("failure messages = %q, want one ending in %q",
			failures, protocol.CommandFailureReadCommand)
	}
}

// splitSpaces splits a command at every space, keeping empty words.
func splitSpaces(command string) []string {
	return strings.Split(command, " ")
}

// TestSessionCommandsHonourInvertedEmptyPattern runs an inverted empty
// pattern through the whole server path: SESSION START builds the read command
// ("grep:... regex:invert,literal "), handleRawCommand splits it and
// dispatchCommand hands its words to the grep command, which a real
// readCommand then runs. It must print no line, not every line.
func TestSessionCommandsHonourInvertedEmptyPattern(t *testing.T) {
	path := writeSharedReadFile(t, "alpha\n\nbeta\n")
	handler, dispatched := newArgsRecordingSessionHandler(t)
	commands := startSession(t, handler, dispatched, session.Spec{
		Mode: omode.GrepClient, Files: []string{path}, RegexInvert: true,
	}, false)
	if len(commands) != 1 {
		t.Fatalf("dispatched %d commands, want 1: %q", len(commands), commands)
	}
	args := commands[0]
	if got := strings.Join(args[2:], " "); got != "regex:invert,literal " {
		t.Fatalf("dispatched regex = %q, want %q", got, "regex:invert,literal ")
	}

	server := newSharedReadTestServer(t, nil)
	cmd := newReadCommandWithDependencies(server.readCommandDependencies(), omode.GrepClient, nil)
	cmd.Start(context.Background(), lcontext.LContext{}, len(args), args, 1)
	if got := drained(t, server); got != "" {
		t.Errorf("output = %q, want no line", got)
	}
	checkReadFailures(t, server.failureMessages(), false)
}

// TestSessionQueryCommandsIgnoreInvert covers an older client which sends
// regex_invert together with a query in SESSION START: dserver reads the lines
// of the query's table, not the other lines.
func TestSessionQueryCommandsIgnoreInvert(t *testing.T) {
	handler, dispatched := newArgsRecordingSessionHandler(t)
	commands := startSession(t, handler, dispatched, session.Spec{
		Mode: omode.MapClient, Files: []string{"/var/log/stats.log"},
		Query: "from STATS select count(*)", Regex: `\|MAPREDUCE:STATS\|`, RegexInvert: true,
	}, false)
	if len(commands) != 2 {
		t.Fatalf("dispatched %d commands, want 2: %q", len(commands), commands)
	}
	re := assertReadRegex(t, commands[1], "/var/log/stats.log", `\|MAPREDUCE:STATS\|`)
	if re.MatchString("INFO|1|MAPREDUCE:OTHER|a=b") || !re.MatchString("INFO|1|MAPREDUCE:STATS|a=b") {
		t.Errorf("query read regex %q is inverted", re.Pattern())
	}
}
