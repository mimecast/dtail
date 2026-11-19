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

// markedFrame is one frame of a server-to-client stream as a client sees
// it: its decoded body, without the control frame marker, and whether it is
// a marked control frame.
type markedFrame struct {
	body    string
	control bool
}

// splitMarkedFrames cuts stream at every MessageDelimiter and decodes each
// frame in sessionFraming, as a client does.
func splitMarkedFrames(stream []byte) []markedFrame {
	var frames []markedFrame
	var scratch []byte
	for len(stream) > 0 {
		end := bytes.IndexByte(stream, protocol.MessageDelimiter)
		if end < 0 {
			break
		}
		body, control := sessionFraming.CutControl(stream[:end])
		frames = append(frames, markedFrame{body: string(sessionFraming.DecodeFrame(body, &scratch)),
			control: control})
		stream = stream[end+1:]
	}
	return frames
}

// unmarkedPayloadOf returns what a client prints: every unmarked frame,
// plain frames verbatim and REMOTE frames by their content field.
func unmarkedPayloadOf(t *testing.T, frames []markedFrame) string {
	t.Helper()
	var payload strings.Builder
	for _, frame := range frames {
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

// TestServerHandlerMarksControlFrames: every control frame of a session, the
// capabilities frame, the AUTHKEY reply and the close handshake included, is
// marked, and no log line is, however much it reads like a control message,
// in plain and REMOTE line format.
func TestServerHandlerMarksControlFrames(t *testing.T) {
	path := writeControlLookalikeFile(t)
	for _, options := range []string{"plain=true", ""} {
		t.Run(options, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runCatSessionAfter(t, handler, path, options, "AUTHKEY abc")
			frames := splitMarkedFrames(stream)
			if !frames[0].control || !strings.HasPrefix(frames[0].body, protocol.HiddenCapabilitiesPrefix) {
				t.Fatalf("first frame = %+v, want the marked capabilities frame", frames[0])
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

// TestServerHandlerMarksAnEarlyClose: a dserver shutting down right after the
// capabilities frame queues its close handshake before the client's
// commands arrive. It goes out marked like every control frame, the client
// acknowledges it, and the session ends without waiting for the
// acknowledgement timeout (5 s).
func TestServerHandlerMarksAnEarlyClose(t *testing.T) {
	handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
	stream := runEarlyCloseSession(t, handler, "AUTHKEY abc")
	assertCloseFrameMarked(t, stream)
}

// assertCloseFrameMarked fails the test unless stream holds exactly one
// close handshake frame, marked, and no marked frame other than a control
// message.
func assertCloseFrameMarked(t *testing.T, stream []byte) {
	t.Helper()
	closeFrames := 0
	for _, frame := range splitMarkedFrames(stream) {
		if frame.body == ".syn close connection" {
			closeFrames++
			if !frame.control {
				t.Fatalf("close handshake not marked: %q", stream)
			}
			continue
		}
		if frame.control && !protocol.IsControlMessage(frame.body) {
			t.Fatalf("frame %q marked as control: %q", frame.body, stream)
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
	// Give Read the time to send the close before the commands arrive.
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
	// The shutdown's flush with nothing queued returns at once instead of
	// waiting for the flush timeout and reporting a flush error.
	if strings.Contains(output.String(), "Unable to flush session output") {
		t.Fatalf("shutdown flush waited for the flush timeout: %q", output.String())
	}
	if elapsed := time.Since(started); elapsed >= defaultOutputFlushTimeout/2 {
		t.Fatalf("session took %s to end, want no wait for a flush or ack timeout", elapsed)
	}
	return []byte(output.String())
}
