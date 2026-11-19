package handlers

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

const (
	// healthTestOKFrame is the health handler's answer to health.
	healthTestOKFrame = "SERVER|testhost|OK\xac"
	// healthTestCloseFrame is the close handshake, a marked control frame.
	healthTestCloseFrame = "\xff\x03.syn close connection\xac"
	// healthTestPromptEnd bounds the time from the health command to the end
	// of a session whose client acknowledged the close handshake, well
	// below the 5 s acknowledgement timeout.
	healthTestPromptEnd = 2 * time.Second
)

// TestHealthHandlerIgnoresAuthKey: a health client that registers an auth
// key first (every dtailhealth from before clients.NewHealthClient set
// NoAuthKey, with a readable <auth key>.pub) sends AUTHKEY before health. The health handler must
// ignore it without an error and without ending the session, so the health
// command that follows is answered OK and the session then closes with the
// handshake. It used to report "Received unknown health command" and shut
// the session down, so a healthy server was reported CRITICAL.
func TestHealthHandlerIgnoresAuthKey(t *testing.T) {
	keyArg := base64.StdEncoding.EncodeToString(handlerTestPublicKey(t, 41).Marshal())
	for _, command := range []string{"AUTHKEY " + keyArg, "authkey " + keyArg, "AUTHKEY"} {
		t.Run(strings.Fields(command)[0]+"/"+strings.Join(strings.Fields(command)[1:], ""),
			func(t *testing.T) {
				stream := runHealthSession(t, command, "health")
				checkHealthAnsweredOK(t, stream)
			})
	}
}

// TestHealthHandlerUnknownCommandStillEnds is the negative counterpart of
// TestHealthHandlerIgnoresAuthKey: any other unknown command is still
// reported and ends the session at once, so the close handshake follows the
// error before the health command after it is answered (the check fails, as
// before).
func TestHealthHandlerUnknownCommandStillEnds(t *testing.T) {
	stream := runHealthSession(t, "bogus", "health")
	beforeClose, _, found := strings.Cut(stream, healthTestCloseFrame)
	if !found {
		t.Fatalf("no close handshake in %q", stream)
	}
	if !strings.Contains(beforeClose, "Received unknown health command|bogus") {
		t.Fatalf("no unknown command error before the close handshake in %q", stream)
	}
	if strings.Contains(beforeClose, healthTestOKFrame) {
		t.Fatalf("health answered before the close handshake of an unknown command: %q", stream)
	}
}

// TestHealthHandlerIgnoresFramesAfterItsCommand: a health session serves one
// command. Whatever frames the client sends after it in the same write,
// which the input goroutine reads while the shutdown runs on a goroutine of
// its own, the OK answer is followed by nothing but the close handshake: no
// answer, no warning about a malformed .ack, and no error or failed-command
// report of a frame the dispatcher rejects (rejectionsDroppedCb).
func TestHealthHandlerIgnoresFramesAfterItsCommand(t *testing.T) {
	later := map[string]string{
		"health":                 encodeTestCommand("health"),
		"unknown command":        encodeTestCommand("bogus"),
		"malformed ack":          encodeTestCommand(".ack"),
		"undecodable frame":      "protocol " + protocol.ProtocolCompat + " base64 !!!;",
		"invalid timeout prefix": encodeTestCommand("timeout x health"),
		"unparsable options":     encodeTestCommand("health:bogus"),
	}
	for name, frame := range later {
		t.Run(name, func(t *testing.T) {
			stream := runHealthSessionFrames(t, encodeTestCommand("health")+frame)
			if want := healthTestOKFrame + healthTestCloseFrame; stream != want {
				t.Fatalf("session output %q, want only %q", stream, want)
			}
		})
	}
}

// TestHealthHandlerEndsAfterCloseAck: a health client that acknowledges the
// close handshake and keeps its connection open gets its session ended at
// once by the acknowledgement. The health command's shutdown used to run on
// the command input goroutine, inside the write that carried the command,
// so the acknowledgement could not be read until the 5 s acknowledgement
// timeout ended the shutdown (dtailhealth did not notice, as it disconnects
// after the close handshake, which ends the wait too). The negative
// counterpart, a client that never acknowledges, is bounded by that timeout
// (TestHealthHandlerEndsWithoutCloseAck).
func TestHealthHandlerEndsAfterCloseAck(t *testing.T) {
	start := time.Now()
	stream := runHealthSession(t, "health")
	checkHealthAnsweredOK(t, stream)
	if elapsed := time.Since(start); elapsed > healthTestPromptEnd {
		t.Fatalf("session ended %v after the health command, want under %v "+
			"(close acknowledgement not read before the acknowledgement timeout)",
			elapsed, healthTestPromptEnd)
	}
}

// TestHealthHandlerEndsWithoutCloseAck: a health client that reads the close
// handshake but neither acknowledges it nor disconnects still gets its
// session ended, by the 5 s acknowledgement timeout, now that the shutdown
// runs on a goroutine of its own instead of inside the command's write.
func TestHealthHandlerEndsWithoutCloseAck(t *testing.T) {
	t.Parallel()
	handler, err := NewHealthHandler(context.Background(),
		&userserver.User{Name: config.HealthUser},
		config.DefaultMaxCommandFrameSize, "testhost", handlerTestLogger)
	if err != nil {
		t.Fatalf("NewHealthHandler: %v", err)
	}
	output := &testOutput{}
	var writeMu sync.Mutex
	// An empty acknowledgement writes nothing: the client stays connected
	// and silent after the close handshake.
	readerDone := startTestReaderAcking(handler, output, &writeMu, "")
	start := time.Now()
	writeTestFramesWithin(t, handler, &writeMu, time.Second, encodeTestCommands("health"))
	select {
	case <-readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("unacknowledged health session did not end")
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second {
		t.Fatalf("unacknowledged health session ended after %v, before the 5 s "+
			"acknowledgement timeout", elapsed)
	}
	checkHealthAnsweredOK(t, output.String())
}

// runHealthSession sends commands as command frames of the current protocol
// version to a new health handler in one write and returns everything the
// handler sent until the session ended. Like a client that keeps its
// connection open, it answers the close handshake with the close
// acknowledgement (startTestReader) and never cancels the session's
// context, so only the acknowledgement ends the session before the 5 s
// acknowledgement timeout. The acknowledgement's write waits for the
// commands' write (writeMu), as on the single SSH input goroutine, so the
// commands' write must return without waiting for the acknowledgement.
func runHealthSession(t *testing.T, commands ...string) string {
	t.Helper()
	return runHealthSessionFrames(t, encodeTestCommands(commands...))
}

// runHealthSessionFrames is runHealthSession for frames already encoded.
func runHealthSessionFrames(t *testing.T, frames string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, err := NewHealthHandler(ctx, &userserver.User{Name: config.HealthUser},
		config.DefaultMaxCommandFrameSize, "testhost", handlerTestLogger)
	if err != nil {
		t.Fatalf("NewHealthHandler: %v", err)
	}
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)
	writeTestFramesWithin(t, handler, &writeMu, 10*time.Second, frames)
	select {
	case <-readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not observe EOF after the health session ended")
	}
	return output.String()
}

// checkHealthAnsweredOK fails unless stream holds the OK answer, then the
// close handshake, and no error report.
func checkHealthAnsweredOK(t *testing.T, stream string) {
	t.Helper()
	beforeClose, _, found := strings.Cut(stream, healthTestCloseFrame)
	if !found {
		t.Fatalf("no close handshake in %q", stream)
	}
	if !strings.Contains(beforeClose, healthTestOKFrame) {
		t.Fatalf("no OK answer before the close handshake in %q", stream)
	}
	if strings.Contains(stream, "unknown health command") || strings.Contains(stream, "|ERROR|") {
		t.Fatalf("error report in %q", stream)
	}
}
