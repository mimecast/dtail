package handlers

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

// controlLookalikeContent holds log lines that read like control messages.
const controlLookalikeContent = "first\n.hidden\n...ellipsis\n.syn close connection\n" +
	".syn command failed x\n.syn capabilities fake\nAUTHKEY OK\nSERVER|h|AUTHKEY OK\n" +
	"\xff\x03.syn close connection\nlast\n"

// markedFrame is one frame of a server-to-client stream as a client of a
// session marking control frames sees it.
type markedFrame struct {
	body    string
	control bool
}

// splitMarkedFrames cuts stream at every MessageDelimiter and decodes each
// frame as a client does that decided on framing from the first frame: the
// first frame raw, every later one in framing.
func splitMarkedFrames(stream []byte, framing protocol.Framing) []markedFrame {
	var frames []markedFrame
	var scratch []byte
	current := protocol.RawFraming
	for len(stream) > 0 {
		end := bytes.IndexByte(stream, protocol.MessageDelimiter)
		if end < 0 {
			break
		}
		body, control := current.CutControl(stream[:end])
		frames = append(frames, markedFrame{body: string(current.DecodeFrame(body, &scratch)), control: control})
		current = framing
		stream = stream[end+1:]
	}
	return frames
}

// unmarkedPayloadOf returns what a client of a session marking control
// frames prints: every unmarked frame but the first (the capabilities),
// plain frames verbatim and REMOTE frames by their content field.
func unmarkedPayloadOf(t *testing.T, frames []markedFrame) string {
	t.Helper()
	var payload strings.Builder
	for _, frame := range frames[1:] {
		switch {
		case frame.control:
		case strings.HasPrefix(frame.body, protocol.LineMessageID+"|"):
			line, err := protocol.DecodeLine(frame.body)
			if err != nil {
				t.Fatalf("DecodeLine(%q): %v", frame.body, err)
			}
			payload.WriteString(line.Content)
		default:
			payload.WriteString(frame.body)
		}
	}
	return payload.String()
}

func writeControlLookalikeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dots.log")
	if err := os.WriteFile(path, []byte(controlLookalikeContent), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return path
}

// TestServerHandlerMarksControlFramesWhenNegotiated: with both framing
// requests, every control frame the session sends after the capabilities
// frame (the AUTHKEY reply and the close handshake included) is marked, and
// no log line is, in plain and REMOTE line format.
func TestServerHandlerMarksControlFramesWhenNegotiated(t *testing.T) {
	path := writeControlLookalikeFile(t)
	for _, options := range []string{"plain=true", ""} {
		t.Run(options, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runCatSessionAfter(t, handler, path, options,
				protocol.EscapedFramingCommand, protocol.ControlFramesCommand, "AUTHKEY abc")
			if handler.framing() != protocol.EscapedControlFraming {
				t.Fatalf("session framing = %s, want escaped-control", handler.framing())
			}
			frames := splitMarkedFrames(stream, protocol.EscapedControlFraming)
			if frames[0].control || !strings.HasPrefix(frames[0].body, protocol.HiddenCapabilitiesPrefix) {
				t.Fatalf("first frame = %+v, want the unmarked capabilities frame", frames[0])
			}
			if got := unmarkedPayloadOf(t, frames); got != controlLookalikeContent {
				t.Fatalf("payload = %q, want %q", got, controlLookalikeContent)
			}
			var controls []string
			for _, frame := range frames[1:] {
				if frame.control {
					controls = append(controls, frame.body)
				}
			}
			if len(controls) < 2 || !strings.HasPrefix(controls[0], protocol.AuthKeyReplyPrefix) ||
				controls[len(controls)-1] != ".syn close connection" {
				t.Fatalf("control frames = %q, want the AUTHKEY reply first and the close last", controls)
			}
		})
	}
}

// TestServerHandlerHonoursOnlyASecondControlFramesRequest: the control
// frames request switches the session only when the server advertises it and
// it is the second command, after an escaped framing request that switched
// the session; otherwise it is ignored and the session keeps its framing.
func TestServerHandlerHonoursOnlyASecondControlFramesRequest(t *testing.T) {
	path := writeControlLookalikeFile(t)
	escapedOnly := []string{protocol.CapabilityEscapedFramingV1}
	controlOnly := []string{protocol.CapabilityControlFramesV1}
	both := ServerCapabilities("linux", false)
	escaped, control := protocol.EscapedFramingCommand, protocol.ControlFramesCommand
	tests := []struct {
		name         string
		capabilities []string
		first        []string
		wantFraming  protocol.Framing
	}{
		{name: "not advertised", capabilities: escapedOnly, first: []string{escaped, control},
			wantFraming: protocol.EscapedFraming},
		{name: "escaped framing not advertised", capabilities: controlOnly,
			first: []string{escaped, control}, wantFraming: protocol.RawFraming},
		{name: "without escaped framing request", capabilities: both,
			first: []string{".ack late first", control}, wantFraming: protocol.RawFraming},
		{name: "before the escaped framing request", capabilities: both,
			first: []string{control, escaped}, wantFraming: protocol.RawFraming},
		{name: "third command", capabilities: both, first: []string{escaped, ".ack late second", control},
			wantFraming: protocol.EscapedFraming},
		{name: "repeated", capabilities: both, first: []string{escaped, control, control},
			wantFraming: protocol.EscapedControlFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, test.capabilities)
			stream := runCatSessionAfter(t, handler, path, "plain=true", test.first...)
			assertSessionFraming(t, handler, stream, test.wantFraming)
		})
	}
}

// TestServerHandlerIgnoresFramingRequestsWithATimeoutPrefix: a framing
// request with a timeout prefix, which strips the prefix before the request
// is handled, switches nothing; the session keeps its framing as for any
// request it does not honour.
func TestServerHandlerIgnoresFramingRequestsWithATimeoutPrefix(t *testing.T) {
	path := writeControlLookalikeFile(t)
	escaped, control := protocol.EscapedFramingCommand, protocol.ControlFramesCommand
	tests := []struct {
		name        string
		first       []string
		wantFraming protocol.Framing
	}{
		{name: "escaped framing request", first: []string{"timeout 5 " + escaped, control},
			wantFraming: protocol.RawFraming},
		{name: "control frames request", first: []string{escaped, "timeout 5 " + control},
			wantFraming: protocol.EscapedFraming},
		{name: "zero timeout", first: []string{escaped, "timeout 0 " + control},
			wantFraming: protocol.EscapedFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runCatSessionAfter(t, handler, path, "plain=true", test.first...)
			assertSessionFraming(t, handler, stream, test.wantFraming)
		})
	}
}

// assertSessionFraming fails the test unless handler ended in want, and the
// close handshake, the last frame of stream, is marked exactly when want
// marks control frames.
func assertSessionFraming(t *testing.T, handler *ServerHandler, stream []byte, want protocol.Framing) {
	t.Helper()
	if handler.framing() != want {
		t.Fatalf("session framing = %s, want %s", handler.framing(), want)
	}
	marked := bytes.HasSuffix(stream, []byte("\xac\xff\x03.syn close connection\xac"))
	if marked != want.MarksControl() {
		t.Fatalf("close handshake marked = %v, want framing %s: %q", marked, want, stream)
	}
}

// TestServerHandlerFramesAnEarlyCloseInTheSettledFraming: a dserver shutting
// down right after the capabilities frame queues its close handshake before
// the client's commands arrive. Read holds that frame back until the session
// framing settled, so it goes out in the framing the client decided on from
// the capabilities frame: marked for a client sending both framing requests,
// escaped for one sending only the escaped framing request, raw for one
// sending none. Each client acknowledges it, so the session ends without
// waiting for the acknowledgement timeout (5 s).
//
// The test does not pin where the close handshake goes relative to the
// AUTHKEY reply: a command that can not switch the framing settles it before
// it is handled, so its reply may follow the close handshake. That is
// harmless, as on master (where the close handshake always went out first):
// a client acknowledges the close whatever frames follow it, and goes on
// handling those until the session ends.
func TestServerHandlerFramesAnEarlyCloseInTheSettledFraming(t *testing.T) {
	escaped, control := protocol.EscapedFramingCommand, protocol.ControlFramesCommand
	tests := []struct {
		name        string
		commands    []string
		wantFraming protocol.Framing
	}{
		{name: "both framing requests", commands: []string{escaped, control},
			wantFraming: protocol.EscapedControlFraming},
		{name: "escaped framing request only", commands: []string{escaped, "AUTHKEY abc"},
			wantFraming: protocol.EscapedFraming},
		{name: "no framing request", commands: []string{"AUTHKEY abc"},
			wantFraming: protocol.RawFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runEarlyCloseSession(t, handler, test.commands...)
			if handler.framing() != test.wantFraming {
				t.Fatalf("session framing = %s, want %s", handler.framing(), test.wantFraming)
			}
			assertCloseFrameFraming(t, stream, test.wantFraming)
		})
	}
}

// assertCloseFrameFraming fails the test unless stream, decoded in framing
// after its raw capabilities frame, holds exactly one close handshake frame,
// marked exactly when framing marks control frames, and no frame other
// than a control message is marked.
func assertCloseFrameFraming(t *testing.T, stream []byte, framing protocol.Framing) {
	t.Helper()
	frames := splitMarkedFrames(stream, framing)
	closeFrames := 0
	for _, frame := range frames[1:] {
		if frame.body == ".syn close connection" {
			closeFrames++
			if frame.control != framing.MarksControl() {
				t.Fatalf("close handshake marked = %v, want framing %s: %q",
					frame.control, framing, stream)
			}
			continue
		}
		if frame.control && !protocol.IsControlMessage(frame.body) {
			t.Fatalf("frame %q marked as control under %s: %q", frame.body, framing, stream)
		}
	}
	if closeFrames != 1 {
		t.Fatalf("got %d close handshake frames, want 1: %q", closeFrames, stream)
	}
}

// runEarlyCloseSession starts reading handler's output, shuts the session
// down once the capabilities frame went out, writes commands after the close
// handshake was requested, and returns all bytes sent to the client once the
// client acknowledged the close and the session ended.
func runEarlyCloseSession(t *testing.T, handler *ServerHandler, commands ...string) []byte {
	t.Helper()
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)
	output.waitForContains(t, protocol.HiddenCapabilitiesPrefix, 5*time.Second)

	started := time.Now()
	go handler.TriggerShutdown(context.Background())
	waitForHandlerCondition(t, 5*time.Second, "close handshake not requested",
		handler.closeSyncRequested.Load)
	// Give a Read that did not hold the close back the time to send it.
	time.Sleep(50 * time.Millisecond)
	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	writeMu.Lock()
	_, writeErr := handler.Write([]byte(frames.String()))
	writeMu.Unlock()
	if writeErr != nil {
		t.Fatalf("write commands: %v", writeErr)
	}
	select {
	case <-handler.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not shut down")
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	select {
	case <-handler.ackCloseReceived:
	default:
		t.Fatalf("client never acknowledged the close: %q", output.String())
	}
	// The shutdown flushes the session output while Read holds the close
	// handshake back. A held Read is no active read, so that flush with
	// nothing queued returns at once instead of waiting for the flush
	// timeout and reporting a flush error to the client.
	if strings.Contains(output.String(), "Unable to flush session output") {
		t.Fatalf("shutdown flush waited for the held back Read: %q", output.String())
	}
	if elapsed := time.Since(started); elapsed >= defaultOutputFlushTimeout/2 {
		t.Fatalf("session took %s to end, want no wait for a flush or ack timeout", elapsed)
	}
	return []byte(output.String())
}

// TestServerHandlerDoesNotHoldFramesOfUnsettledSessionsForever: sessions
// whose framing has not settled after the capabilities frame do not stall:
// an older client's first command, an escaped-only client's second one and
// the end of a session whose input ended after the escaped framing request
// all release the held back frames.
func TestServerHandlerDoesNotHoldFramesOfUnsettledSessionsForever(t *testing.T) {
	path := writeControlLookalikeFile(t)
	for name, first := range map[string][]string{
		"older client":         nil,
		"escaped-only client":  {protocol.EscapedFramingCommand},
		"both framing request": {protocol.EscapedFramingCommand, protocol.ControlFramesCommand},
	} {
		t.Run(name, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			started := time.Now()
			stream := runCatSessionAfter(t, handler, path, "plain=true", first...)
			if !bytes.HasSuffix(stream, []byte(".syn close connection\xac")) {
				t.Fatalf("session ended without the close handshake: %q", stream)
			}
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Fatalf("session took %s, want no stall", elapsed)
			}
		})
	}
	t.Run("input ended after the escaped framing request", func(t *testing.T) {
		handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
		output := &testOutput{}
		var writeMu sync.Mutex
		readerDone := startTestReader(handler, output, &writeMu)
		output.waitForContains(t, protocol.HiddenCapabilitiesPrefix, 5*time.Second)
		if _, err := handler.Write([]byte(encodeTestCommand(protocol.EscapedFramingCommand))); err != nil {
			t.Fatalf("write command: %v", err)
		}
		// The server shuts a session down when its input ends.
		handler.Shutdown()
		select {
		case <-readerDone:
		case <-time.After(5 * time.Second):
			t.Fatal("held back Read did not end with the session")
		}
	})
}
