package handlers

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// rejectedFrame is a command frame the dispatcher rejects before any handler
// runs (commandDispatcher.handleCommand), with the message that reports it,
// whether the session then ends with the close handshake, and the frames the
// client answers that handshake with.
type rejectedFrame struct {
	frame     string
	report    string
	handshake bool
	ack       string
}

// mismatchReport is the error message of a frame of protocol version 99.0.
const mismatchReport = "does not match client protocol version '99.0'"

// encodeMismatchCommand frames command like a client of protocol version
// 99.0, which this server does not speak.
func encodeMismatchCommand(command string) string {
	return "protocol 99.0 base64 " + base64.StdEncoding.EncodeToString([]byte(command)) + ";"
}

// rejectedFrames are the command frames commandDispatcher.handleCommand
// rejects before any handler runs, each with what reports the rejection to
// the client: the failed-command message, or for a protocol version
// mismatch, which gets none, its error message. A session of another
// protocol version ends without the close handshake
// (commandDispatcher.rejectProtocolVersion); its client here sends its
// command twice, and the second frame must get no error message, as the
// session is closing then.
func rejectedFrames() map[string]rejectedFrame {
	ack := encodeTestCommand(".ack close connection")
	mismatch := encodeMismatchCommand("cat test.log .")
	return map[string]rejectedFrame{
		"invalid timeout prefix": {encodeTestCommand("timeout x cat test.log ."),
			protocol.HiddenCommandFailedPrefix + commandFailureRejected, true, ack},
		"unparsable options": {encodeTestCommand("cat:bogus test.log ."),
			protocol.HiddenCommandFailedPrefix + commandFailureRejected, true, ack},
		"undecodable frame": {"protocol " + protocol.ProtocolCompat + " base64 !!!;",
			protocol.HiddenCommandFailedPrefix + commandFailureProtocol, true, ack},
		"protocol version mismatch": {mismatch + mismatch, mismatchReport, false,
			encodeMismatchCommand(".ack close connection")},
	}
}

// checkRejectedFrameSession fails unless stream, all bytes of a session
// whose only command frame was rejected, holds the rejection's report
// exactly once, followed, if the rejection ends the session with the close
// handshake, by exactly one close handshake and nothing after it, and
// otherwise by no close handshake at all (checkMismatchSession).
func checkRejectedFrameSession(t *testing.T, stream string, rejected rejectedFrame) {
	t.Helper()
	if !rejected.handshake {
		checkMismatchSession(t, stream)
		return
	}
	if closes := strings.Count(stream, ".syn close connection"); closes != 1 {
		t.Fatalf("%d close handshakes, want 1: %q", closes, stream)
	}
	beforeClose, afterClose, _ := strings.Cut(stream, ".syn close connection")
	if !strings.Contains(beforeClose, rejected.report) {
		t.Fatalf("rejection report %q missing before the close handshake: %q",
			rejected.report, stream)
	}
	if reports := strings.Count(stream, rejected.report); reports != 1 {
		t.Fatalf("%d rejection reports, want 1: %q", reports, stream)
	}
	// The close handshake's frame ends with the message delimiter.
	if strings.TrimSuffix(afterClose, string([]byte{protocol.MessageDelimiter})) != "" {
		t.Fatalf("output after the close handshake: %q", afterClose)
	}
}

// checkMismatchSession fails unless stream, all bytes of a session whose
// command frames were all of another protocol version, ends with the
// mismatch error, sent exactly once, and holds no close handshake: the
// server ends such a session without it (closeWithoutHandshake).
func checkMismatchSession(t *testing.T, stream string) {
	t.Helper()
	if strings.Contains(stream, ".syn close connection") {
		t.Fatalf("close handshake in a session of another protocol version: %q", stream)
	}
	if reports := strings.Count(stream, mismatchReport); reports != 1 {
		t.Fatalf("%d mismatch errors, want 1: %q", reports, stream)
	}
	_, afterReport, _ := strings.Cut(stream, mismatchReport)
	if want := ", please update DTail server" + string([]byte{protocol.MessageDelimiter}); afterReport != want {
		t.Fatalf("output after the mismatch error %q, want %q", afterReport, want)
	}
}

// checkCloseAcknowledgement fails unless handler received the client's close
// acknowledgement, if the rejection ends the session with the close
// handshake, or did not receive one, if it ends it without.
func checkCloseAcknowledgement(t *testing.T, ackCloseReceived <-chan struct{},
	rejected rejectedFrame, stream string) {
	t.Helper()
	select {
	case <-ackCloseReceived:
		if !rejected.handshake {
			t.Fatalf("close acknowledged in a session without close handshake: %q", stream)
		}
	default:
		if rejected.handshake {
			t.Fatalf("session ended without the close acknowledgement: %q", stream)
		}
	}
}

// TestServerHandlerRejectedOnlyCommandEndsSession: a session whose only
// command frame is rejected before any handler ran admits no command, so no
// command finishes it; the dispatcher's rejection hook
// (ServerHandler.frameRejected) starts its idle shutdown instead. Before, such
// a session sent its failure report but no close handshake and stayed open
// until IdleSessionTimeoutS. The session must end well under a second
// (runFailingFramesSession), with the report before exactly one close
// handshake and nothing after it, for older, escaped-only and control-frame
// clients. A command of another protocol version ends the session as
// quickly, after its error but without the close handshake
// (commandDispatcher.rejectProtocolVersion); see also
// TestServerHandlerVersionMismatchEndsSessionWithoutHandshake.
func TestServerHandlerRejectedOnlyCommandEndsSession(t *testing.T) {
	clients := map[string][]string{
		"control-frame client": {protocol.EscapedFramingCommand, protocol.ControlFramesCommand},
	}
	for client, first := range settleTestClients {
		clients[client] = first
	}
	for client, first := range clients {
		for name, rejected := range rejectedFrames() {
			t.Run(client+"/"+name, func(t *testing.T) {
				handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
				stream := runFailingFramesSessionAcking(t, handler, protocol.HiddenCapabilitiesPrefix,
					encodeTestCommands(first...)+rejected.frame, rejected.ack)
				checkRejectedFrameSession(t, stream, rejected)
				checkCloseAcknowledgement(t, handler.ackCloseReceived, rejected, stream)
			})
		}
	}
}

// TestServerHandlerVersionMismatchEndsSessionWithoutHandshake: every frame
// of a client of another protocol version is rejected, including the
// framing requests a current client sends first, so the session stays raw
// while such a client already chose marked framing from the capabilities
// frame: it would print an unmarked ".syn close connection" as payload and
// never acknowledge it, so the session waited for the close acknowledgement
// timeout (5 s). The server must instead send the mismatch error once and
// end the session at once without the close handshake
// (ServerHandler.versionRejected), for older, escaped-only and control-frame
// clients of that version, which then see an incomplete session.
func TestServerHandlerVersionMismatchEndsSessionWithoutHandshake(t *testing.T) {
	clients := map[string][]string{
		"older client":         nil,
		"escaped-only client":  {protocol.EscapedFramingCommand},
		"control-frame client": {protocol.EscapedFramingCommand, protocol.ControlFramesCommand},
	}
	for client, first := range clients {
		t.Run(client, func(t *testing.T) {
			var frames strings.Builder
			for _, command := range append(append([]string(nil), first...), "cat test.log .") {
				frames.WriteString(encodeMismatchCommand(command))
			}
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runFailingFramesSessionAcking(t, handler, protocol.HiddenCapabilitiesPrefix,
				frames.String(), encodeMismatchCommand(".ack close connection"))
			_, afterCapabilities, _ := strings.Cut(stream, protocol.HiddenCapabilitiesPrefix)
			checkMismatchSession(t, afterCapabilities)
			if handler.framing() != protocol.RawFraming {
				t.Fatalf("framing %v after rejected framing requests, want raw", handler.framing())
			}
		})
	}
}

// TestServerHandlerRejectedCommandInBatchDoesNotEndSessionEarly: a frame
// rejected inside an open input batch must not start the idle shutdown
// (claimIdleShutdown sees the open batch), so the cat that follows it in the
// batch delivers all of its lines before exactly one close handshake.
func TestServerHandlerRejectedCommandInBatchDoesNotEndSessionEarly(t *testing.T) {
	cat := idleShutdownBatchCatCommand(t, writeIdleShutdownBatchFile(t))
	for name, rejected := range rejectedFrames() {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 5; i++ {
				stream := runBatchedFailingSession(t, rejected.frame, cat)
				if closes := strings.Count(stream, ".syn close connection"); closes != 1 {
					t.Fatalf("iteration %d: %d close handshakes, want 1", i, closes)
				}
				beforeClose, _, _ := strings.Cut(stream, ".syn close connection")
				if !strings.Contains(beforeClose, rejected.report) {
					t.Fatalf("iteration %d: rejection report %q missing before the close handshake",
						i, rejected.report)
				}
				checkAllCatLines(t, beforeClose)
			}
		})
	}
}

// TestHealthHandlerRejectedFrameEndsSession: a health session whose command
// frame is rejected before any handler ran ends at once
// (HealthHandler.frameRejected), with the rejection's report before exactly
// one close handshake and nothing after it; for a client of another protocol
// version (HealthHandler.versionRejected), after the mismatch error, without
// the close handshake. The health client reports CRITICAL either way, as it
// gets no health OK. Before, no handler ran and the session stayed open
// until the client gave up.
func TestHealthHandlerRejectedFrameEndsSession(t *testing.T) {
	for name, rejected := range rejectedFrames() {
		t.Run(name, func(t *testing.T) {
			handler, err := NewHealthHandler(context.Background(), &userserver.User{Name: "health"},
				config.DefaultMaxCommandFrameSize, "testhost", handlerTestLogger)
			if err != nil {
				t.Fatalf("NewHealthHandler: %v", err)
			}
			stream := runFailingFramesSessionAcking(t, handler, "", rejected.frame, rejected.ack)
			checkRejectedFrameSession(t, stream, rejected)
			checkCloseAcknowledgement(t, handler.ackCloseReceived, rejected, stream)
		})
	}
}

// TestServerHandlerRejectedFrameKeepsBusySessionOpen: a frame rejected while
// a SESSION START keeps the session alive, or while an unbatched tail still
// runs, must not close the session (claimIdleShutdown sees the kept-alive
// session or the active command): its report arrives, but no close
// handshake follows.
func TestServerHandlerRejectedFrameKeepsBusySessionOpen(t *testing.T) {
	path := writeIdleShutdownBatchFile(t)
	busy := map[string]func(t *testing.T, handler *ServerHandler, output *testOutput,
		writeMu *sync.Mutex){
		"SESSION START": func(t *testing.T, handler *ServerHandler, output *testOutput, writeMu *sync.Mutex) {
			start, err := session.Spec{Mode: omode.CatClient, Files: []string{path}, Regex: "."}.StartCommand()
			if err != nil {
				t.Fatalf("build SESSION START: %v", err)
			}
			writeTestCommandsWithin(t, handler, writeMu, 10*time.Second, start)
			output.waitForContains(t, sessionAckStartOKPrefix, 5*time.Second)
		},
		"running tail": func(t *testing.T, handler *ServerHandler, _ *testOutput, writeMu *sync.Mutex) {
			commands, err := session.Spec{Mode: omode.TailClient, Files: []string{path}, Regex: "."}.Commands()
			if err != nil || len(commands) != 1 {
				t.Fatalf("build tail command: %v (%d commands)", err, len(commands))
			}
			writeTestCommandsWithin(t, handler, writeMu, 10*time.Second, commands[0])
			waitForActiveCommand(t, handler)
		},
	}
	for busyName, makeBusy := range busy {
		for name, rejected := range rejectedFrames() {
			t.Run(busyName+"/"+name, func(t *testing.T) {
				handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
				output := &testOutput{}
				var writeMu sync.Mutex
				readerDone := startTestReader(handler, output, &writeMu)
				output.waitForContains(t, protocol.HiddenCapabilitiesPrefix, 5*time.Second)

				makeBusy(t, handler, output, &writeMu)
				writeTestFramesWithin(t, handler, &writeMu, 10*time.Second, rejected.frame)
				output.waitForContains(t, rejected.report, 5*time.Second)
				// A wrongly started idle shutdown sends its close handshake
				// at once; give it time to show.
				time.Sleep(200 * time.Millisecond)
				if strings.Contains(output.String(), ".syn close connection") {
					t.Fatalf("rejected frame closed a busy session: %q", output.String())
				}
				handler.Shutdown()
				select {
				case <-readerDone:
				case <-time.After(5 * time.Second):
					t.Fatal("reader did not observe EOF after handler shutdown")
				}
			})
		}
	}
}

// waitForActiveCommand waits until handler runs a command.
func waitForActiveCommand(t *testing.T, handler *ServerHandler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, active := handler.PendingAndActive(); active > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// olderMismatchReport is the error message of a frame of protocol version
// 3.9, followed by the newline such an older client expects after it.
const olderMismatchReport = "does not match client protocol version '3.9', please update DTail client\n"

// noVersionReport is the error message of a frame without a protocol prefix,
// as v1.x clients send them. It is followed by no newline: a v1.x client
// never got one.
const noVersionReport = "unable to determine protocol version"

// encodeOlderMismatchCommand frames command like an older client of
// protocol version 3.9.
func encodeOlderMismatchCommand(command string) string {
	return "protocol 3.9 base64 " + base64.StdEncoding.EncodeToString([]byte(command)) + ";"
}

// encodeV1Command frames command like a v1.x client: no protocol prefix and
// no base64 encoding, just "%s;".
func encodeV1Command(command string) string {
	return command + ";"
}

// legacyClient is how a legacy client frames its commands and the mismatch
// error the server answers them with.
type legacyClient struct {
	encode func(command string) string
	report string
}

// TestOlderVersionMismatchKeepsSessionOpen: a session of a legacy client
// (protocol <= 3, or a v1.x client sending no protocol prefix at all) whose
// frames are all rejected for their protocol version gets the mismatch
// error once, followed by the separator such a client expects (a newline
// for protocol 2-3, none for v1.x), and then stays open, as it did before
// (commandDispatcher.rejectProtocolVersion): it ends when the client exits
// or at IdleSessionTimeoutS. Legacy clients that retry (dtail of every such
// version, dmap in tail mode without outfile) reconnect 2 s after their
// session ended, so ending it at once would make them reconnect in a loop.
// Both the server and the health handler must keep such a session open with
// no close handshake.
func TestOlderVersionMismatchKeepsSessionOpen(t *testing.T) {
	clients := map[string]legacyClient{
		"protocol 3.9":       {encode: encodeOlderMismatchCommand, report: olderMismatchReport},
		"no protocol prefix": {encode: encodeV1Command, report: noVersionReport},
	}
	for clientName, client := range clients {
		for handlerName, newHandler := range legacyTestHandlers() {
			t.Run(clientName+"/"+handlerName, func(t *testing.T) {
				checkLegacySessionStaysOpen(t, client, newHandler)
			})
		}
	}
}

// legacyTestHandlers returns constructors of the server and the health
// handler, each with the function that shuts it down.
func legacyTestHandlers() map[string]func(t *testing.T) (io.ReadWriter, func()) {
	return map[string]func(t *testing.T) (io.ReadWriter, func()){
		"server": func(t *testing.T) (io.ReadWriter, func()) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			return handler, handler.Shutdown
		},
		"health": func(t *testing.T) (io.ReadWriter, func()) {
			handler, err := NewHealthHandler(context.Background(), &userserver.User{Name: "health"},
				config.DefaultMaxCommandFrameSize, "testhost", handlerTestLogger)
			if err != nil {
				t.Fatalf("NewHealthHandler: %v", err)
			}
			return handler, handler.Shutdown
		},
	}
}

// checkLegacySessionStaysOpen sends two command frames of client to a new
// handler and checks that its session stays open with a single mismatch
// error, and that it ends once the handler shuts down.
func checkLegacySessionStaysOpen(t *testing.T, client legacyClient,
	newHandler func(t *testing.T) (io.ReadWriter, func())) {
	t.Helper()
	handler, shutdown := newHandler(t)
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReaderAcking(handler, output, &writeMu,
		client.encode(".ack close connection"))
	frame := client.encode("cat test.log .")
	writeTestFramesWithin(t, handler, &writeMu, 10*time.Second, frame+frame)
	output.waitForContains(t, client.report, 5*time.Second)
	checkOlderMismatchSessionOpen(t, output, readerDone, client.report)
	shutdown()
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
}

// checkOlderMismatchSessionOpen fails unless the session of a legacy client
// whose frames were all of another protocol version is still open 500 ms
// after its mismatch error report, which it got exactly once, and sent no
// close handshake.
func checkOlderMismatchSessionOpen(t *testing.T, output *testOutput,
	readerDone <-chan struct{}, report string) {
	t.Helper()
	select {
	case <-readerDone:
		t.Fatalf("session of a legacy client ended after the mismatch error: %q", output.String())
	case <-time.After(500 * time.Millisecond):
	}
	stream := output.String()
	if strings.Contains(stream, ".syn close connection") {
		t.Fatalf("close handshake in a session of a legacy client: %q", stream)
	}
	if reports := strings.Count(stream, report); reports != 1 {
		t.Fatalf("%d mismatch errors, want 1: %q", reports, stream)
	}
}
