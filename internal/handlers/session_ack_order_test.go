package handlers

// Tests that dserver sends the SESSION START/UPDATE acknowledgement ahead of
// the payload of the generation it acknowledges (task fd4). The session
// output delivers payload before server messages (task 1c4), and the
// acknowledgement used to be one of them, queued after the read commands
// were dispatched: on a slow link it then waited behind the whole session
// output buffer (megabytes), and the client's 2 s acknowledgement wait
// (connectors.defaultSessionAckTimeout) ran out.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
)

// clientAckWait is the client's acknowledgement wait
// (connectors.defaultSessionAckTimeout), which the acknowledgement must
// reach the client within, even on a slow link.
const clientAckWait = 2 * time.Second

func TestSessionAckPrecedesPayloadOnSlowLink(t *testing.T) {
	for _, serverless := range []bool{false, true} {
		name := "dserver"
		if serverless {
			name = "serverless"
		}
		t.Run(name, func(t *testing.T) {
			testSessionAckPrecedesPayloadOnSlowLink(t, serverless)
		})
	}
}

func testSessionAckPrecedesPayloadOnSlowLink(t *testing.T, serverless bool) {
	stream, payload := &testOutput{}, &testOutput{}
	options := "plain=true"
	var handler *ServerHandler
	if serverless {
		handler = newMapTestHandlerWithOutput(t, handlerTestLogger, payload)
		options += ":serverless=true"
	} else {
		handler = newMapTestHandlerWithOutput(t, handlerTestLogger, nil)
		payload = stream
	}
	path := writeAckOrderFile(t, 60000)
	commandWg := wrapHandlerCommandsForJoin(handler)
	var writeMu sync.Mutex
	link := startSlowLinkReader(handler, stream, &writeMu)

	// Generation 1 floods the session output with A lines.
	start := session.Spec{Mode: omode.CatClient, Files: []string{path}, Options: options, Regex: "^A "}
	writeModeSwapCommand(t, handler, &writeMu, mustStartCommand(t, start))
	startAck := sessionAckStartOKPrefix + " 1"
	waitForAckWithin(t, stream, startAck, clientAckWait)
	payload.waitForContains(t, "A 000", 10*time.Second)
	if !serverless {
		requireBefore(t, stream.String(), startAck, "A 000")
	}

	// Generation 2 replaces it with B lines while A lines still fill the
	// output buffer.
	update := start
	update.Regex = "^B "
	writeModeSwapCommand(t, handler, &writeMu, mustUpdateCommand(t, update, 2))
	updateAck := sessionAckUpdateOKPrefix + " 2"
	waitForAckWithin(t, stream, updateAck, clientAckWait)
	payload.waitForContains(t, "B 000", 10*time.Second)
	if !serverless {
		requireBefore(t, stream.String(), updateAck, "B 000")
		// The replaced generation's queued output is dropped: no A line
		// starts after the acknowledgement of its successor.
		if after := textAfter(stream.String(), updateAck); strings.Contains(after, "A 0") {
			t.Fatalf("output of the replaced generation after %q", updateAck)
		}
	}
	requireRejectedUpdateAckInTime(t, handler, stream, &writeMu, update)

	link.fast()
	handler.Shutdown()
	link.waitDone(t, 30*time.Second)
	waitForCommandJoin(t, commandWg, 10*time.Second)
}

// requireRejectedUpdateAckInTime sends a SESSION UPDATE the server rejects
// while generation 2 (running) still fills the output buffer with B lines.
// The rejection replaces nothing, so that payload stays deliverable; the
// error acknowledgement must still reach the client within its wait (it
// goes ahead of the payload, tryReadSessionAckFirst), and only between two
// frames.
func requireRejectedUpdateAckInTime(t *testing.T, handler *ServerHandler, stream *testOutput,
	writeMu *sync.Mutex, running session.Spec) {

	t.Helper()
	rejected := running
	rejected.Mode = omode.MapClient // without a query: "missing session query"
	writeModeSwapCommand(t, handler, writeMu, mustUpdateCommand(t, rejected, 3))
	errorAck := sessionAckErrorPrefix + "missing session query"
	waitForAckWithin(t, stream, errorAck, clientAckWait)
	text := stream.String()
	at := strings.Index(text, "\xff\x03"+errorAck)
	if at < 1 || text[at-1] != protocol.MessageDelimiter {
		t.Fatalf("%q does not start right after a frame delimiter", errorAck)
	}
}

// writeAckOrderFile writes lines lines alternating between "A <n> ..." and
// "B <n> ...", about 100 bytes each.
func writeAckOrderFile(t *testing.T, lines int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ackorder.log")
	pad := strings.Repeat("x", 88)
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		kind := "A"
		if i%2 == 1 {
			kind = "B"
		}
		fmt.Fprintf(&sb, "%s %06d %s\n", kind, i, pad)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

// waitForAckWithin fails unless output contains ack within wait.
func waitForAckWithin(t *testing.T, output *testOutput, ack string, wait time.Duration) {
	t.Helper()
	started := time.Now()
	output.waitForContains(t, ack, wait)
	t.Logf("%q arrived after %s", ack, time.Since(started).Round(time.Millisecond))
}

// requireBefore fails unless first occurs in text before second.
func requireBefore(t *testing.T, text, first, second string) {
	t.Helper()
	firstAt, secondAt := strings.Index(text, first), strings.Index(text, second)
	if firstAt < 0 || (secondAt >= 0 && secondAt < firstAt) {
		t.Fatalf("%q at %d does not precede %q at %d", first, firstAt, second, secondAt)
	}
}

// slowLinkReader drains handler.Read like the SSH session output goroutine
// over a slow link: 32 KiB (the SSH copy's buffer) every 100 ms, about
// 320 KiB/s, until fast is called. It answers the close handshake like
// startTestReader.
type slowLinkReader struct {
	slow atomic.Bool
	done chan struct{}
}

func startSlowLinkReader(handler io.ReadWriter, output *testOutput, writeMu *sync.Mutex) *slowLinkReader {
	link := &slowLinkReader{done: make(chan struct{})}
	link.slow.Store(true)
	ack := encodeTestCommand(".ack close connection")
	var ackOnce sync.Once
	go func() {
		defer close(link.done)
		p := make([]byte, 32*1024)
		for {
			n, err := handler.Read(p)
			if n > 0 && strings.Contains(output.append(p[:n]), ".syn close connection") {
				ackOnce.Do(func() {
					writeMu.Lock()
					defer writeMu.Unlock()
					_, _ = handler.Write([]byte(ack))
				})
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if link.slow.Load() {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	return link
}

func (l *slowLinkReader) fast() { l.slow.Store(false) }

func (l *slowLinkReader) waitDone(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-l.done:
	case <-time.After(timeout):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
}
