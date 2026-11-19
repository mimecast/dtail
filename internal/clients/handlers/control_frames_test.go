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

// controlFramesCapabilities is the capabilities frame a protocol 5 server
// without journal support starts a session with; it lists no capability.
func controlFramesCapabilities() []byte {
	return capabilitiesFrame()
}

// controlFrames encodes messages as marked control frames, as the server
// sends them.
func controlFrames(messages ...string) []byte {
	var stream bytes.Buffer
	for _, message := range messages {
		protocol.EscapedControlFraming.EncodeControl(&stream, message)
	}
	return stream.Bytes()
}

// TestClientHandlerPrintsControlLookalikePayload: every session marks
// control frames, so every unmarked frame is payload, so --plain log lines that
// read like control messages are printed byte for byte and change neither
// the session outcome nor the capabilities.
func TestClientHandlerPrintsControlLookalikePayload(t *testing.T) {
	stream := append(controlFramesCapabilities(),
		payloadFrames(protocol.EscapedControlFraming, controlLookalikeLines...)...)
	for _, size := range []int{1, 2, 7, len(stream)} {
		recorder := &payloadRecorder{}
		handler := NewClientHandler("srv1", recorder)
		writeInChunks(t, handler, stream, size)

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
	stream = append(stream, controlFrames(
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

// TestMaprHandlerWithControlFrames: a mapreduce session, whose control
// frames are marked like those of every session, still aggregates its AGGREGATE frames and handles its control
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
	stream.Write(controlFrames(
		protocol.HiddenCommandFailedPrefix + protocol.CommandFailureNoFile + "\n"))
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
