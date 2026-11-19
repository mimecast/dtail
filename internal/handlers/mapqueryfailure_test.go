package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
)

// duplicateSelectQuery is a query older clients send but dserver rejects.
const duplicateSelectQuery = "from STATS select count($time),count($time) group by $time"

// TestFailedMapCommandRejectsLaterReads sends the legacy command stream of
// an older client whose query dserver rejects: the read commands following
// the failed map command must not stream the file as plain payload, each
// reports the failure, and the session ends with its close handshake.
func TestFailedMapCommandRejectsLaterReads(t *testing.T) {
	for _, mode := range []omode.Mode{omode.MapClient, omode.TailClient} {
		t.Run(mode.String(), func(t *testing.T) {
			output := runLegacyCommandStream(t, session.Spec{
				Mode:  mode,
				Files: []string{writeTestStatsFile(t, 25)},
				Query: duplicateSelectQuery,
				Regex: ".",
			})
			if strings.Contains(output, "MAPREDUCE:STATS") {
				t.Fatalf("read of a session with a failed map command streamed payload: %q", output)
			}
			for _, want := range []string{
				"'select' clause has 'count($time)' more than once",
				protocol.HiddenCommandFailedPrefix + protocol.CommandFailureMapQuery,
				".syn close connection",
			} {
				if !strings.Contains(output, want) {
					t.Fatalf("output lacks %q: %q", want, output)
				}
			}
		})
	}
}

// echoErrorLogger returns its Error arguments as the message, which the
// handlers' NopLogger drops, so a test can see the diagnostic a client gets.
type echoErrorLogger struct{ logging.NopLogger }

func (echoErrorLogger) Error(args ...any) string { return fmt.Sprint(args...) }

// TestFailedMapCommandNamesTheDecodedPath checks that the diagnostic of a
// read rejected after a failed map command names a spaced path as the user
// gave it, not in the base64% form it travels in.
func TestFailedMapCommandNamesTheDecodedPath(t *testing.T) {
	const spacedPath = "/var/log/my app/a.log"
	handler, _ := buildLimiterTestHandler(t, 1)
	handler.logger = echoErrorLogger{}
	read := newReadCommand(handler, omode.CatClient)
	read.rejectAfterMapQueryFailure(context.Background(),
		[]string{"cat:", protocol.EncodeReadPath(spacedPath), "regex"})

	message := readServerMessage(t, handler.serverMessages)
	if !strings.Contains(message, "Not reading, the session's map query failed") ||
		!strings.Contains(message, spacedPath) {
		t.Fatalf("diagnostic does not name %q: %q", spacedPath, message)
	}
}

// TestRejectedReadPath checks the file word shown for a rejected read: an
// encoded path decoded, a plain one and one that does not decode as sent.
func TestRejectedReadPath(t *testing.T) {
	const spacedPath = "/var/log/my app/a.log"
	invalid := protocol.EncodedReadPathPrefix + "!!!"
	for _, tt := range []struct {
		args []string
		want string
	}{
		{args: []string{"cat:"}, want: ""},
		{args: []string{"cat:", "/var/log/app.log", "regex"}, want: "/var/log/app.log"},
		{args: []string{"cat:", protocol.EncodeReadPath(spacedPath), "regex"}, want: spacedPath},
		{args: []string{"cat:", invalid, "regex"}, want: invalid},
	} {
		if got := rejectedReadPath(tt.args); got != tt.want {
			t.Errorf("rejectedReadPath(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// runLegacyCommandStream writes spec's commands in one input batch, as a
// client does, waits until the session ended and returns its output.
func runLegacyCommandStream(t *testing.T, spec session.Spec) string {
	t.Helper()
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	return runLegacyCommands(t, commands)
}

// runLegacyCommands is runLegacyCommandStream for the given commands.
func runLegacyCommands(t *testing.T, commands []string) string {
	t.Helper()
	handler := newMapTestHandler(t)
	commandWg := wrapHandlerCommandsForJoin(handler)
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)

	var frames strings.Builder
	frames.WriteString(encodeTestCommand(protocol.InputBatchBeginCommand))
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	frames.WriteString(encodeTestCommand(protocol.InputBatchCompleteCommand))
	writeMu.Lock()
	_, err := handler.Write([]byte(frames.String()))
	writeMu.Unlock()
	if err != nil {
		t.Fatalf("write commands: %v", err)
	}

	select {
	case <-handler.Done():
	case <-time.After(20 * time.Second):
		t.Fatalf("session did not end after its map command failed: %q", output.String())
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	waitForCommandJoin(t, commandWg, 10*time.Second)
	return output.String()
}

// TestMapQueryFailuresAreScopedToTheirGeneration checks that a failed map
// command of one command stream (legacy generation 0 or a session
// generation) does not reject the reads of another.
func TestMapQueryFailuresAreScopedToTheirGeneration(t *testing.T) {
	var failures mapQueryFailures
	if failures.failed(0) || failures.failed(1) {
		t.Fatal("no map command failed yet")
	}
	failures.mark(0)
	if !failures.failed(0) || failures.failed(1) {
		t.Fatal("a failure of the legacy stream must reject only its reads")
	}
	failures.mark(2)
	if failures.failed(0) || failures.failed(1) || !failures.failed(2) {
		t.Fatal("a failure of generation 2 must reject only its reads")
	}
	failures.clear(1)
	if !failures.failed(2) {
		t.Fatal("a successful map command of generation 1 must keep the failure of generation 2")
	}
	failures.clear(2)
	if failures.failed(2) {
		t.Fatal("a successful map command of generation 2 must clear its failure")
	}
}

// TestSessionStartRejectsDuplicateSelect checks the interactive bootstrap:
// a SESSION START whose query selects a field twice is rejected with the
// reason, and none of its commands are dispatched.
func TestSessionStartRejectsDuplicateSelect(t *testing.T) {
	handler, recorder := newQuerySessionDispatchTestHandler("session-duplicate-select-user")
	readServerMessage(t, handler.serverMessages)

	payload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Query: duplicateSelectQuery,
		Regex: ".",
	})
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", payload}, func() {})

	assertDuplicateSelectRejected(t, handler)
	if handler.sessionState.activeSession() {
		t.Fatal("a rejected SESSION START must not start the session")
	}
	assertNoDispatchedCommand(t, recorder)
}

// TestSessionUpdateRejectsDuplicateSelect checks :reload: an update whose
// query selects a field twice is rejected with the reason, while the running
// generation keeps running untouched.
func TestSessionUpdateRejectsDuplicateSelect(t *testing.T) {
	handler, recorder := newQuerySessionDispatchTestHandler("session-reload-duplicate-select-user")
	readServerMessage(t, handler.serverMessages)

	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Query: "from STATS select count($time) group by $time",
		Regex: ".",
	}
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", mustSessionPayload(t, spec)}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	started := []recordedCommand{recorder.waitForStart(t), recorder.waitForStart(t)}

	spec.Query = duplicateSelectQuery
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 4,
		[]string{"SESSION", "UPDATE", "2", mustSessionPayload(t, spec)}, func() {})

	assertDuplicateSelectRejected(t, handler)
	if generation := handler.sessionState.currentGeneration(); generation != 1 {
		t.Fatalf("generation = %d after a rejected update, want 1", generation)
	}
	for _, command := range started {
		if command.ctx.Err() != nil {
			t.Fatalf("a rejected update cancelled the running command %q", command.command)
		}
	}
	assertNoDispatchedCommand(t, recorder)
}

// TestSessionRejectsAFailedMapCommand covers a map command of a session
// that fails although its query passed validation (e.g. when creating its
// aggregate): the request is rejected and its reads are not dispatched.
func TestSessionRejectsAFailedMapCommand(t *testing.T) {
	handler, recorder := newQuerySessionDispatchTestHandler("session-failed-map-user")
	readServerMessage(t, handler.serverMessages)
	handler.commands["map"] = func(ctx context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
		handler.mapQueryFailures.mark(sessionGenerationFromContext(ctx))
		commandFinished()
	}

	payload := mustSessionPayload(t, session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Query: "from STATS select count($time) group by $time",
		Regex: ".",
	})
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", payload}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != sessionAckErrorPrefix+"invalid session query" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	if handler.sessionState.activeSession() {
		t.Fatal("a session whose map command failed must not stay started")
	}
	assertNoDispatchedCommand(t, recorder)
}

func assertDuplicateSelectRejected(t *testing.T, handler *ServerHandler) {
	t.Helper()
	message := readServerMessage(t, handler.serverMessages)
	if !strings.HasPrefix(message, sessionAckErrorPrefix+"invalid session spec: ") ||
		!strings.Contains(message, "'select' clause has 'count($time)' more than once") {
		t.Fatalf("unexpected session error: %q", message)
	}
}

func assertNoDispatchedCommand(t *testing.T, recorder *sessionDispatchRecorder) {
	t.Helper()
	select {
	case command := <-recorder.starts:
		t.Fatalf("unexpected dispatched command %q", command.command)
	default:
	}
}

// TestRejectedMapCommandRejectsLaterReads covers a map command that
// dispatchCommand rejects before handleMapCommand runs, here for options
// that do not parse: its reads must be rejected as well instead of
// streaming the file as plain payload.
func TestRejectedMapCommandRejectsLaterReads(t *testing.T) {
	commands, err := session.Spec{
		Mode:  omode.MapClient,
		Files: []string{writeTestStatsFile(t, 25)},
		Query: "from STATS select count($time) group by $time",
		Regex: ".",
	}.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	if !strings.HasPrefix(commands[0], "map: ") {
		t.Fatalf("unexpected map command %q", commands[0])
	}
	commands[0] = "map:bogus" + strings.TrimPrefix(commands[0], "map:")

	output := runLegacyCommands(t, commands)
	if strings.Contains(output, "MAPREDUCE:STATS") {
		t.Fatalf("read after a rejected map command streamed payload: %q", output)
	}
	for _, want := range []string{
		protocol.HiddenCommandFailedPrefix + protocol.CommandFailureRejected,
		protocol.HiddenCommandFailedPrefix + protocol.CommandFailureMapQuery,
		".syn close connection",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q: %q", want, output)
		}
	}
}

// failingMapParserFactory makes building a map command fail after its query
// parsed, which no query a client sends does as things stand.
func failingMapParserFactory(string, *mapr.Query, string) (logformat.Parser, error) {
	return nil, errors.New("forced parser failure")
}

// TestSessionStartRejectsAMapCommandThatCanNotBeBuilt checks that a SESSION
// START whose map command can not be built is rejected with the reason before
// any of its commands is dispatched.
func TestSessionStartRejectsAMapCommandThatCanNotBeBuilt(t *testing.T) {
	handler, recorder := newQuerySessionDispatchTestHandler("session-unbuildable-map-user")
	readServerMessage(t, handler.serverMessages)
	handler.mapParserFactory = failingMapParserFactory

	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", mustSessionPayload(t, querySessionSpec())}, func() {})

	assertUnbuildableMapRejected(t, handler)
	if handler.sessionState.activeSession() {
		t.Fatal("a rejected SESSION START must not start the session")
	}
	assertNoDispatchedCommand(t, recorder)
}

// TestSessionUpdateRejectsAMapCommandThatCanNotBeBuilt checks :reload with a
// map command that can not be built: the update is rejected before the
// running generation is cancelled, which keeps running, so that the next
// :reload is still accepted.
func TestSessionUpdateRejectsAMapCommandThatCanNotBeBuilt(t *testing.T) {
	handler, recorder := newQuerySessionDispatchTestHandler("session-reload-unbuildable-map-user")
	readServerMessage(t, handler.serverMessages)
	spec := querySessionSpec()
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", mustSessionPayload(t, spec)}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	started := []recordedCommand{recorder.waitForStart(t), recorder.waitForStart(t)}

	handler.mapParserFactory = failingMapParserFactory
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 4,
		[]string{"SESSION", "UPDATE", "2", mustSessionPayload(t, spec)}, func() {})

	assertUnbuildableMapRejected(t, handler)
	if generation := handler.sessionState.currentGeneration(); generation != 1 {
		t.Fatalf("generation = %d after a rejected update, want 1", generation)
	}
	for _, command := range started {
		if command.ctx.Err() != nil {
			t.Fatalf("a rejected update cancelled the running command %q", command.command)
		}
	}
	assertNoDispatchedCommand(t, recorder)

	handler.mapParserFactory = nil
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "UPDATE", mustSessionPayload(t, spec)}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 2" {
		t.Fatalf("unexpected session update ack after a rejected update: %q", message)
	}
}

// TestSessionClearsAStaleMapQueryFailure checks that a failure left by an
// earlier command stream of the same generation, e.g. of a session rejected
// after its map command failed, does not reject the reads of a later SESSION
// START or UPDATE of that generation that has no query.
func TestSessionClearsAStaleMapQueryFailure(t *testing.T) {
	handler := newSessionTestHandler("session-stale-map-failure-user")
	readServerMessage(t, handler.serverMessages)
	rejected := make(chan bool, 4)
	handler.commands["tail"] = func(ctx context.Context, _ lcontext.LContext, _ int, _ []string, commandFinished func()) {
		rejected <- handler.mapQueryFailures.failed(sessionGenerationFromContext(ctx))
		commandFinished()
	}
	spec := session.Spec{Mode: omode.TailClient, Files: []string{"/var/log/app.log"}, Regex: "."}

	handler.mapQueryFailures.mark(1)
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 3,
		[]string{"SESSION", "START", mustSessionPayload(t, spec)}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckStartOKPrefix+" 1" {
		t.Fatalf("unexpected session start ack: %q", message)
	}
	if <-rejected {
		t.Fatal("a stale failure of generation 1 rejected a read of a new SESSION START")
	}

	handler.mapQueryFailures.mark(5)
	handler.handleSessionCommand(context.Background(), lcontext.LContext{}, 4,
		[]string{"SESSION", "UPDATE", "5", mustSessionPayload(t, spec)}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != sessionAckUpdateOKPrefix+" 5" {
		t.Fatalf("unexpected session update ack: %q", message)
	}
	if <-rejected {
		t.Fatal("a stale failure of generation 5 rejected a read of SESSION UPDATE 5")
	}
}

// TestMapCommandDiagnosticKeepsTheFraming checks that the error a failed map
// command sends, which quotes the query, is sanitized for raw framing only:
// EscapedFraming and EscapedControlFraming both escape every frame body.
func TestMapCommandDiagnosticKeepsTheFraming(t *testing.T) {
	args := strings.Split("map: from STATS select count(a€),count(a€) group by $time", " ")
	framings := []protocol.Framing{protocol.RawFraming, protocol.EscapedFraming,
		protocol.EscapedControlFraming}
	for _, framing := range framings {
		escaped := framing != protocol.RawFraming
		handler := newSessionTestHandler("map-diagnostic-user")
		readServerMessage(t, handler.serverMessages)
		handler.framingState.Store(uint32(framing))
		handler.handleMapCommand(context.Background(), lcontext.LContext{}, len(args), args, func() {})

		message := readServerMessage(t, handler.serverMessages)
		hasDelimiter := strings.IndexByte(message, protocol.MessageDelimiter) >= 0
		if !strings.Contains(message, "more than once") || hasDelimiter == !escaped {
			t.Fatalf("framing %v: unexpected map command diagnostic %q", framing, message)
		}
	}
}

func querySessionSpec() session.Spec {
	return session.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Query: "from STATS select count($time) group by $time",
		Regex: ".",
	}
}

func assertUnbuildableMapRejected(t *testing.T, handler *ServerHandler) {
	t.Helper()
	message := readServerMessage(t, handler.serverMessages)
	if !strings.HasPrefix(message, sessionAckErrorPrefix+"invalid session query: ") ||
		!strings.Contains(message, "forced parser failure") {
		t.Fatalf("unexpected session error: %q", message)
	}
}
