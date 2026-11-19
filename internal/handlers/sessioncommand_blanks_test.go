package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
	"github.com/mimecast/dtail/internal/session"
)

// newArgsRecordingSessionHandler returns a session handler whose read and
// map commands record the argument slices they are dispatched with, so a
// test can rebuild the regex the way readCommand.Start does.
func newArgsRecordingSessionHandler(t *testing.T) (*ServerHandler, chan []string) {
	t.Helper()

	handler := newSessionTestHandler("session-blanks-user")
	readServerMessage(t, handler.serverMessages)
	dispatched := make(chan []string, 8)
	record := func(_ context.Context, _ lcontext.LContext, _ int, args []string, commandFinished func()) {
		dispatched <- append([]string(nil), args...)
		commandFinished()
	}
	for _, name := range []string{"cat", "grep", "tail", "map"} {
		handler.commands[name] = record
	}
	return handler, dispatched
}

// startSession sends spec as SESSION START (or as SESSION UPDATE after a
// START when update is set) and returns the read command's arguments.
func startSession(t *testing.T, handler *ServerHandler, dispatched chan []string,
	spec session.Spec, update bool) [][]string {
	t.Helper()

	verb, ack := "START", sessionAckStartOKPrefix
	if update {
		handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
			[]string{"SESSION", "START", mustSessionPayload(t, session.Spec{
				Mode: omode.GrepClient, Files: []string{"/var/log/old.log"}, Regex: "old",
			})}, func() {})
		readServerMessage(t, handler.serverMessages)
		<-dispatched
		verb, ack = "UPDATE", sessionAckUpdateOKPrefix
	}
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", verb, mustSessionPayload(t, spec)}, func() {})
	if message := readServerMessage(t, handler.serverMessages); !strings.HasPrefix(message, ack) {
		t.Fatalf("session %s answered %q, want prefix %q", verb, message, ack)
	}

	var commands [][]string
	for {
		select {
		case args := <-dispatched:
			commands = append(commands, args)
		default:
			return commands
		}
	}
}

// TestSessionCommandsKeepBlanksInTheRegex checks that the blanks of a regex
// reach the read command unchanged through SESSION START and UPDATE, as they
// do through the legacy base64 command path: a trailing blank, a run of
// blanks, a tab and a leading blank.
func TestSessionCommandsKeepBlanksInTheRegex(t *testing.T) {
	patterns := []string{"foo ", "x  y", "a\tb", " lead", "x  y  "}
	for _, update := range []bool{false, true} {
		for _, pattern := range patterns {
			name := "start"
			if update {
				name = "update"
			}
			t.Run(name+"/"+pattern, func(t *testing.T) {
				handler, dispatched := newArgsRecordingSessionHandler(t)
				commands := startSession(t, handler, dispatched, session.Spec{
					Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: pattern,
				}, update)
				if len(commands) != 1 {
					t.Fatalf("dispatched %d commands, want 1: %q", len(commands), commands)
				}
				assertReadRegex(t, commands[0], "/var/log/app.log", pattern)
			})
		}
	}
}

// TestSessionQueryCommandsKeepBlanks covers the query workload: the map
// query and the "timeout N" prefixed read command keep their blanks too.
func TestSessionQueryCommandsKeepBlanks(t *testing.T) {
	handler, dispatched := newArgsRecordingSessionHandler(t)
	query := `from STATS select count(*) where $line eq "a  b"`
	commands := startSession(t, handler, dispatched, session.Spec{
		Mode: omode.TailClient, Files: []string{"/var/log/app.log"},
		Query: query, Regex: "x  y\t", Timeout: 5,
	}, false)
	if len(commands) != 2 {
		t.Fatalf("dispatched %d commands, want 2: %q", len(commands), commands)
	}
	if got := strings.Join(commands[0][1:], " "); got != query {
		t.Errorf("map query = %q, want %q", got, query)
	}
	assertReadRegex(t, commands[1], "/var/log/app.log", "x  y\t")
}

// TestSessionCommandsWithoutRegexMatchEverything checks the empty pattern:
// the builder serializes it as "regex:noop " and the trailing empty field
// rejoins to a regex matching every line, as on the legacy path.
func TestSessionCommandsWithoutRegexMatchEverything(t *testing.T) {
	handler, dispatched := newArgsRecordingSessionHandler(t)
	commands := startSession(t, handler, dispatched, session.Spec{
		Mode: omode.CatClient, Files: []string{"/var/log/app.log"},
	}, false)
	if len(commands) != 1 {
		t.Fatalf("dispatched %d commands, want 1: %q", len(commands), commands)
	}
	re := assertReadRegex(t, commands[0], "/var/log/app.log", "")
	for _, line := range []string{"", "anything", "x  y"} {
		if !re.MatchString(line) {
			t.Errorf("empty session regex does not match %q", line)
		}
	}
}

// assertReadRegex checks a read command's file argument and rebuilds its
// regex like readCommand.Start, from the rejoined args[2:].
func assertReadRegex(t *testing.T, args []string, wantFile, wantPattern string) regex.Regex {
	t.Helper()

	if len(args) < 4 {
		t.Fatalf("read command args = %q, want at least 4", args)
	}
	if args[1] != wantFile {
		t.Errorf("read command file = %q, want %q", args[1], wantFile)
	}
	re, err := regex.Deserialize(strings.Join(args[2:], " "))
	if err != nil {
		t.Fatalf("deserialize regex of %q: %v", args, err)
	}
	if re.Pattern() != wantPattern {
		t.Errorf("read command regex = %q, want %q", re.Pattern(), wantPattern)
	}
	return re
}
