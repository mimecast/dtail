package handlers

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
)

// TestServerHandlerSessionStartWithManyMissingFilesDoesNotDeadlock: a
// SESSION START whose reads queue more "Unable to read file(s)" messages
// than the server message channel holds is acknowledged, and the messages
// all reach the client: while the workload is held for its acknowledgement
// (sessionAckGate), Read keeps draining the channel into the deferred
// messages, so neither the reads nor the command input goroutine block, and
// none of them goes out ahead of the acknowledgement.
func TestServerHandlerSessionStartWithManyMissingFilesDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 60)
	for i := range files {
		files[i] = filepath.Join(dir, fmt.Sprintf("missing%d.log", i))
	}
	start, err := session.Spec{Mode: omode.CatClient, Files: files, Regex: "."}.StartCommand()
	if err != nil {
		t.Fatalf("build SESSION START: %v", err)
	}
	handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)
	output.waitForContains(t, protocol.HiddenCapabilitiesPrefix, 5*time.Second)

	writeTestCommandsWithin(t, handler, &writeMu, 10*time.Second, start)
	output.waitForContains(t, sessionAckStartOKPrefix, 5*time.Second)
	// The reports of the workload follow its acknowledgement
	// (sessionAckGate), failure reports included.
	waitForAfter(t, output, sessionAckStartOKPrefix, "Unable to read file(s)", 5*time.Second)
	waitForAfter(t, output, sessionAckStartOKPrefix, protocol.HiddenCommandFailedPrefix, 5*time.Second)
	before, _, _ := strings.Cut(output.String(), sessionAckStartOKPrefix)
	if strings.Contains(before, "Unable to read file(s)") ||
		strings.Contains(before, protocol.HiddenCommandFailedPrefix) {
		t.Fatalf("output of the workload ahead of its acknowledgement: %q", before)
	}
	handler.Shutdown()
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
}

// TestServerHandlerFailingOnlyCommandsDoNotWaitForTimeouts: an unknown
// command or a failed map query that is a session's only command shuts the
// session down, flushing its output and then running the close handshake.
// The flush does not wait for a Read held back until the command returned,
// which would take the full flush timeout and report a bogus flush error, also
// when former framing requests (now unknown ".ack" commands) come first. The
// shutdown runs off
// the command input goroutine (ServerHandler.finishUserCommand), so the
// client's close acknowledgement, a later command frame, is handled at once
// instead of after the acknowledgement timeout (5 s).
func TestServerHandlerFailingOnlyCommandsDoNotWaitForTimeouts(t *testing.T) {
	commands := map[string]string{
		"unknown command":  "bogus command",
		"failed map query": "map: from STATS select count(a),count(a) group by $time",
	}
	clients := map[string][]string{
		"no ack first":           nil,
		"former framing request": {".ack framing escaped-v1", ".ack framing control-v1"},
	}
	for client, first := range clients {
		for name, command := range commands {
			t.Run(client+"/"+name, func(t *testing.T) {
				handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
				stream := runFailingCommandSession(t, handler, first, command)
				if strings.Contains(stream, "Unable to flush session output") {
					t.Fatalf("shutdown flush waited for a held back Read: %q", stream)
				}
				closeAt := strings.Index(stream, ".syn close connection")
				if closeAt < 0 {
					t.Fatalf("session ended without the close handshake: %q", stream)
				}
				// The close handshake still comes after all of the
				// session's output, here the command's failure report.
				if failedAt := strings.Index(stream, protocol.HiddenCommandFailedPrefix); failedAt < 0 || failedAt > closeAt {
					t.Fatalf("failure report missing or after the close handshake: %q", stream)
				}
				select {
				case <-handler.ackCloseReceived:
				default:
					t.Fatalf("session ended without the close acknowledgement: %q", stream)
				}
			})
		}
	}
}

// runFailingCommandSession sends the first command frames and then command,
// like runFailingFramesSession.
func runFailingCommandSession(t *testing.T, handler *ServerHandler, first []string,
	command string) string {
	t.Helper()
	return runFailingFramesSession(t, handler,
		encodeTestCommands(append(append([]string(nil), first...), command)...))
}

// runFailingFramesSession writes frames, raw command frames as a client
// sends them, acknowledges the close handshake with an ".ack close
// connection" command frame like a client, checks that the session ends well
// under a second, i.e. without waiting for the flush timeout (2 s) or the
// close acknowledgement timeout (5 s), and returns all bytes sent to the
// client.
func runFailingFramesSession(t *testing.T, handler *ServerHandler, frames string) string {
	t.Helper()
	return runFailingFramesSessionAcking(t, handler, protocol.HiddenCapabilitiesPrefix,
		frames, encodeTestCommand(".ack close connection"))
}

// runFailingFramesSessionAcking is runFailingFramesSession, but answers the
// close handshake with ack (startTestReaderAcking), and waits for greeting,
// unless empty, before it writes frames: a ServerHandler first sends its
// capabilities, a HealthHandler nothing.
func runFailingFramesSessionAcking(t *testing.T, handler io.ReadWriter,
	greeting, frames, ack string) string {
	t.Helper()
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReaderAcking(handler, output, &writeMu, ack)
	if greeting != "" {
		output.waitForContains(t, greeting, 5*time.Second)
	}

	started := time.Now()
	writeTestFramesWithin(t, handler, &writeMu, 10*time.Second, frames)
	select {
	case <-readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("session took %s to end, want no wait for a flush or ack timeout", elapsed)
	}
	return output.String()
}

// writeTestCommandsWithin writes commands as command frames in one write,
// like writeTestFramesWithin.
func writeTestCommandsWithin(t *testing.T, handler *ServerHandler, writeMu *sync.Mutex,
	timeout time.Duration, commands ...string) {
	t.Helper()
	writeTestFramesWithin(t, handler, writeMu, timeout, encodeTestCommands(commands...))
}

// encodeTestCommands returns commands as command frames, like a client
// sends them.
func encodeTestCommands(commands ...string) string {
	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	return frames.String()
}

// writeTestFramesWithin writes frames in one write and fails the test unless
// the write returns within timeout, i.e. unless the command input goroutine
// handled them all.
func writeTestFramesWithin(t *testing.T, handler io.Writer, writeMu *sync.Mutex,
	timeout time.Duration, frames string) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := handler.Write([]byte(frames))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write commands: %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("command input goroutine blocked handling the commands")
	}
}
