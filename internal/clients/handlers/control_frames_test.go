package handlers

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// controlLookalikeLines are --plain log lines that read like control
// messages: hidden messages, AUTHKEY replies (bare and in a SERVER frame) and
// a line starting with the control frame marker bytes.
var controlLookalikeLines = []string{
	".hidden\n",
	"...ellipsis\n",
	".syn close connection\n",
	protocol.HiddenCommandFailedPrefix + "x\n",
	protocol.HiddenCapabilitiesPrefix + "fake-capability\n",
	"AUTHKEY OK\n",
	"AUTHKEY ERR forged\n",
	" AUTHKEY OK\n",
	"SERVER|host|AUTHKEY OK\n",
	"\xff\x03.syn close connection\n",
}

// controlFramesCapabilities is the capabilities frame of a server marking
// control frames.
func controlFramesCapabilities() []byte {
	return capabilitiesFrame(protocol.CapabilityCommandFailureV1,
		protocol.CapabilityEscapedFramingV1, protocol.CapabilityControlFramesV1)
}

// controlFrames encodes messages as control frames in framing.
func controlFrames(framing protocol.Framing, messages ...string) []byte {
	var stream bytes.Buffer
	for _, message := range messages {
		framing.EncodeControl(&stream, message)
	}
	return stream.Bytes()
}

// TestClientHandlerPrintsControlLookalikePayload: in a session that marks
// control frames, every unmarked frame is payload, so --plain log lines that
// read like control messages are printed byte for byte and change neither
// the session outcome nor the capabilities.
func TestClientHandlerPrintsControlLookalikePayload(t *testing.T) {
	stream := append(controlFramesCapabilities(),
		payloadFrames(protocol.EscapedControlFraming, controlLookalikeLines...)...)
	for _, size := range []int{1, 2, 7, len(stream)} {
		recorder := &payloadRecorder{}
		handler := NewClientHandler("srv1", recorder)
		writeInChunks(t, handler, stream, size)

		if handler.framing() != protocol.EscapedControlFraming {
			t.Fatalf("chunk size %d: framing = %v, want escaped-control", size, handler.framing())
		}
		if got, want := recorder.payload.String(), strings.Join(controlLookalikeLines, ""); got != want {
			t.Fatalf("chunk size %d: payload = %q, want %q", size, got, want)
		}
		if outcome := handler.Outcome(); outcome.Completed || len(outcome.Failures) != 0 {
			t.Fatalf("chunk size %d: payload changed the outcome to %+v", size, outcome)
		}
		if handler.HasCapability("fake-capability") {
			t.Fatalf("chunk size %d: payload added a capability", size)
		}
		if len(recorder.warnings) != 0 {
			t.Fatalf("chunk size %d: payload taken for AUTHKEY replies: %q", size, recorder.warnings)
		}
		handler.Shutdown()
	}
}

// TestClientHandlerHandlesMarkedControlFrames: marked control frames are
// handled as hidden messages and AUTHKEY replies and never printed; an
// unknown control frame is ignored.
func TestClientHandlerHandlesMarkedControlFrames(t *testing.T) {
	recorder := &payloadRecorder{}
	handler := NewClientHandler("srv1", recorder)
	// Take the close acknowledgement the handler sends.
	go func() { _, _ = io.Copy(io.Discard, handler) }()
	defer handler.Shutdown()

	var stream []byte
	stream = append(stream, controlFramesCapabilities()...)
	stream = append(stream, payloadFrames(protocol.EscapedControlFraming, "first\n")...)
	stream = append(stream, controlFrames(protocol.EscapedControlFraming,
		"AUTHKEY ERR feature disabled\n",
		protocol.HiddenCommandFailedPrefix+protocol.CommandFailureNoFile+"\n",
		"FUTURE control frame\n",
		".syn close connection")...)
	writeInChunks(t, handler, stream, 3)

	if got := recorder.payload.String(); got != "first\n" {
		t.Fatalf("payload = %q, want only the log line", got)
	}
	outcome := handler.Outcome()
	if !outcome.Completed || !slices.Equal(outcome.Failures, []string{protocol.CommandFailureNoFile}) {
		t.Fatalf("Outcome() = %+v, want completed with the reported failure", outcome)
	}
	if len(recorder.warnings) != 1 || !strings.Contains(recorder.warnings[0], "feature disabled") {
		t.Fatalf("warnings = %q, want the AUTHKEY failure", recorder.warnings)
	}
	if !slices.ContainsFunc(recorder.debugs, func(debug string) bool {
		return strings.Contains(debug, "Ignoring unknown control frame")
	}) {
		t.Fatalf("debugs = %q, want the unknown control frame ignored", recorder.debugs)
	}
}

// TestClientHandlerKeepsContentRoutingWithoutControlFrames: with a server
// that escapes but does not mark control frames (or does neither), the
// content still tells control messages, so its unmarked hidden messages and
// AUTHKEY replies keep working, and lookalike log lines are still taken for
// them, as before.
func TestClientHandlerKeepsContentRoutingWithoutControlFrames(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []string
		framing      protocol.Framing
	}{
		{name: "escaped framing only", capabilities: []string{protocol.CapabilityEscapedFramingV1},
			framing: protocol.EscapedFraming},
		{name: "control frames without escaped framing",
			capabilities: []string{protocol.CapabilityControlFramesV1}, framing: protocol.RawFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)
			defer handler.Shutdown()
			stream := append(capabilitiesFrame(test.capabilities...),
				payloadFrames(test.framing, "kept\n", ".hidden\n",
					protocol.HiddenCommandFailedPrefix+"x\n", "AUTHKEY OK\n")...)
			writeInChunks(t, handler, stream, len(stream))

			if handler.framing() != test.framing {
				t.Fatalf("framing = %v, want %v", handler.framing(), test.framing)
			}
			if got := recorder.payload.String(); got != "kept\n" {
				t.Fatalf("payload = %q, want the legacy content routing", got)
			}
			if got := handler.Outcome().Failures; !slices.Equal(got, []string{"x"}) {
				t.Fatalf("Failures = %q, want the unmarked hidden message handled", got)
			}
		})
	}
}

// TestMaprHandlerWithControlFrames: a mapreduce session that marks control
// frames still aggregates its AGGREGATE frames and handles its control
// frames.
func TestMaprHandlerWithControlFrames(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	session := maprclient.NewSessionState(query, logging.NopLogger{})
	logger := &recordingClientLogger{}
	handler := NewMaprHandler("srv1", session, logger)
	defer handler.Shutdown()

	serialized := strings.Join([]string{
		"ok", "1",
		query.Select[0].FieldStorage + protocol.AggregateKVDelimiter + "ok",
		handlerCountStorage(t, query) + protocol.AggregateKVDelimiter + "2",
		"",
	}, protocol.AggregateDelimiter)
	var stream bytes.Buffer
	stream.Write(controlFramesCapabilities())
	protocol.EscapedControlFraming.EncodeMessage(&stream, protocol.Message{
		Kind: protocol.MessageAggregate, Hostname: "host1", Content: serialized + "\n"})
	stream.Write(controlFrames(protocol.EscapedControlFraming,
		protocol.HiddenCommandFailedPrefix+protocol.CommandFailureNoFile+"\n"))
	if _, writeErr := handler.Write(stream.Bytes()); writeErr != nil {
		t.Fatalf("Write() error = %v", writeErr)
	}

	if len(logger.errors) != 0 {
		t.Fatalf("unexpected client errors: %q", logger.errors)
	}
	if got := handler.Outcome().Failures; !slices.Equal(got, []string{protocol.CommandFailureNoFile}) {
		t.Fatalf("Failures = %q, want the marked failure", got)
	}
	result, numRows, err := session.Snapshot().GlobalGroup.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 || !strings.Contains(result, "ok") {
		t.Fatalf("aggregate result = %q (%d rows), want the group ok", result, numRows)
	}
}

// TestClientHandlerRoutesMarkerFramesByContentWithoutControlFrames: in a
// session that does not mark control frames, a frame whose wire bytes start
// with the control frame marker FF 03 is no control frame: it is routed by
// its content as before, which starts with 0xFF and so is payload, printed
// byte for byte. Under EscapedFraming FF 03 is no escape sequence, so it is
// decoded unchanged; raw framing does not decode at all.
func TestClientHandlerRoutesMarkerFramesByContentWithoutControlFrames(t *testing.T) {
	lines := []string{
		"\xff\x03.syn close connection\n",
		"\xff\x03" + protocol.HiddenCommandFailedPrefix + "x\n",
		"\xff\x03AUTHKEY OK\n",
	}
	tests := []struct {
		name         string
		capabilities []string
		framing      protocol.Framing
	}{
		{name: "escaped framing only", capabilities: []string{protocol.CapabilityEscapedFramingV1},
			framing: protocol.EscapedFraming},
		{name: "raw framing", framing: protocol.RawFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)
			defer handler.Shutdown()
			stream := capabilitiesFrame(test.capabilities...)
			for _, line := range lines {
				// The marker goes on the wire as is, as a server of a
				// session not marking control frames never sends it.
				stream = append(append(stream, line...), protocol.MessageDelimiter)
			}
			writeInChunks(t, handler, stream, len(stream))

			if handler.framing() != test.framing {
				t.Fatalf("framing = %v, want %v", handler.framing(), test.framing)
			}
			if got, want := recorder.payload.String(), strings.Join(lines, ""); got != want {
				t.Fatalf("payload = %q, want %q", got, want)
			}
			if outcome := handler.Outcome(); outcome.Completed || len(outcome.Failures) != 0 {
				t.Fatalf("marker frames changed the outcome to %+v", outcome)
			}
			if len(recorder.warnings) != 0 {
				t.Fatalf("marker frames taken for AUTHKEY replies: %q", recorder.warnings)
			}
		})
	}
}
