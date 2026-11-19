package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
)

// idleShutdownBatchLines is the size of the file the batched cat reads: big
// enough that the read is still running when a wrongly started idle
// shutdown closes the session.
const idleShutdownBatchLines = 5000

// TestServerHandlerFailingCommandInBatchDoesNotEndSessionEarly: a failing
// command (unknown command, failed map query) inside an input batch finishes
// on the command input goroutine while the batch is open, so it must not
// start the idle shutdown, also not once the batch completed while a later
// cat of the batch still runs. The check and the claim of the idle shutdown
// happen synchronously in finishUserCommand (claimIdleShutdown); before, the
// claim ran later on the shutdown goroutine and could close the session
// while the cat was still reading. Every iteration has to deliver all of the
// cat's output before exactly one close handshake.
func TestServerHandlerFailingCommandInBatchDoesNotEndSessionEarly(t *testing.T) {
	path := writeIdleShutdownBatchFile(t)
	cat := idleShutdownBatchCatCommand(t, path)
	cases := map[string]struct {
		failing string
		check   func(t *testing.T, beforeClose string)
	}{
		"unknown command": {"bogus command", checkAllCatLines},
		// The map query fails before the cat is dispatched, so the cat is
		// rejected; its rejection is the output that must precede the close.
		"failed map query": {"map: from STATS select count(a),count(a) group by $time",
			func(t *testing.T, beforeClose string) {
				t.Helper()
				if !strings.Contains(beforeClose, "Not reading, the session's map query failed") {
					t.Fatalf("rejected cat not reported before the close handshake: %q", beforeClose)
				}
			}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 10; i++ {
				stream := runBatchedFailingSession(t, encodeTestCommand(tc.failing), cat)
				if closes := strings.Count(stream, ".syn close connection"); closes != 1 {
					t.Fatalf("iteration %d: %d close handshakes, want 1", i, closes)
				}
				beforeClose, _, _ := strings.Cut(stream, ".syn close connection")
				tc.check(t, beforeClose)
			}
		})
	}
}

// runBatchedFailingSession sends failing, a raw command frame, and cat
// inside one input batch, waits for the session to end and returns all
// bytes sent to the client.
func runBatchedFailingSession(t *testing.T, failing, cat string) string {
	t.Helper()
	handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)
	output.waitForContains(t, protocol.HiddenCapabilitiesPrefix, 5*time.Second)

	frames := encodeTestCommand(protocol.InputBatchBeginCommand) + failing +
		encodeTestCommands(cat, protocol.InputBatchCompleteCommand)
	writeTestFramesWithin(t, handler, &writeMu, 10*time.Second, frames)
	select {
	case <-readerDone:
	case <-time.After(20 * time.Second):
		t.Fatal("reader did not observe EOF after the session ended")
	}
	return output.String()
}

// checkAllCatLines fails unless every line of the cat's file arrived, in
// order, before the close handshake.
func checkAllCatLines(t *testing.T, beforeClose string) {
	t.Helper()
	pos := 0
	for i := 0; i < idleShutdownBatchLines; i++ {
		want := fmt.Sprintf("line %d\n", i)
		at := strings.Index(beforeClose[pos:], want)
		if at < 0 {
			t.Fatalf("line %d of %d did not arrive before the close handshake", i, idleShutdownBatchLines)
		}
		pos += at + len(want)
	}
}

func writeIdleShutdownBatchFile(t *testing.T) string {
	t.Helper()
	var content strings.Builder
	for i := 0; i < idleShutdownBatchLines; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	path := filepath.Join(t.TempDir(), "batch.log")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

func idleShutdownBatchCatCommand(t *testing.T, path string) string {
	t.Helper()
	commands, err := session.Spec{Mode: omode.CatClient, Files: []string{path}, Regex: "."}.Commands()
	if err != nil || len(commands) != 1 {
		t.Fatalf("build cat command: %v (%d commands)", err, len(commands))
	}
	return commands[0]
}
