package handlers

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// framingTestLines contain characters whose UTF-8 encoding holds the
// MessageDelimiter byte 0xAC ('€', 'Ь', '¬') and the escape byte 0xFF.
var framingTestLines = []string{
	"price 5€ ok\n",
	"soft Ь sign\n",
	"not ¬ sign\n",
	"byte \xff\xac end\n",
}

// capabilitiesFrame is the capabilities frame a protocol 5 server starts a
// session with: a marked control frame.
func capabilitiesFrame(capabilities ...string) []byte {
	var stream bytes.Buffer
	protocol.EscapedControlFraming.EncodeControl(&stream,
		protocol.HiddenCapabilitiesPrefix+strings.Join(capabilities, " "))
	return stream.Bytes()
}

// unmarkedCapabilitiesFrame is the capabilities frame of a server of an
// older protocol version: an unmarked, unescaped frame.
func unmarkedCapabilitiesFrame(capabilities ...string) []byte {
	return payloadFrames(protocol.TerminalFraming,
		protocol.HiddenCapabilitiesPrefix+strings.Join(capabilities, " "))
}

func payloadFrames(framing protocol.Framing, lines ...string) []byte {
	var stream bytes.Buffer
	for _, line := range lines {
		framing.EncodePayload(&stream, []byte(line))
	}
	return stream.Bytes()
}

func readFirstFrame(t *testing.T, handler *baseHandler) string {
	t.Helper()
	p := make([]byte, 4096)
	n, err := handler.Read(p)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return string(p[:n])
}

// TestClientHandlersSendNoFramingRequest: protocol 5 negotiates no framing,
// so the first command frame of a client or mapreduce session is its first
// command, framed in the current protocol version.
func TestClientHandlersSendNoFramingRequest(t *testing.T) {
	want := "protocol " + protocol.ProtocolCompat + " base64 " +
		base64.StdEncoding.EncodeToString([]byte("AUTHKEY abc")) + ";"

	client := NewClientHandler("srv1", nil)
	defer client.Shutdown()
	go func() { _ = client.SendMessage("AUTHKEY abc") }()
	if got := readFirstFrame(t, &client.baseHandler); got != want {
		t.Fatalf("first client frame = %q, want %q", got, want)
	}

	query, err := mapr.NewQuery("select count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	mapClient := NewMaprHandler("srv1", maprclient.NewSessionState(query, logging.NopLogger{}), nil)
	defer mapClient.Shutdown()
	go func() { _ = mapClient.SendMessage("AUTHKEY abc") }()
	if got := readFirstFrame(t, &mapClient.baseHandler); got != want {
		t.Fatalf("first mapr frame = %q, want %q", got, want)
	}
}

// TestClientHandlerDecodesEscapedFrames: every frame after the capabilities
// frame is decoded as escaped, so lines holding 0xAC or 0xFF bytes arrive
// whole and byte for byte, however the stream is chunked.
func TestClientHandlerDecodesEscapedFrames(t *testing.T) {
	stream := append(capabilitiesFrame(protocol.CapabilityJournalV1),
		payloadFrames(protocol.EscapedControlFraming, framingTestLines...)...)
	for _, size := range []int{1, 3, len(stream)} {
		recorder := &payloadRecorder{}
		handler := NewClientHandler("srv1", recorder)
		writeInChunks(t, handler, stream, size)
		if got, want := recorder.payload.String(), strings.Join(framingTestLines, ""); got != want {
			t.Fatalf("chunk size %d: payload = %q, want %q", size, got, want)
		}
		if handler.Status() > 0 {
			t.Fatalf("chunk size %d: status = %d, want no error", size, handler.Status())
		}
		if !handler.HasCapability(protocol.CapabilityJournalV1) {
			t.Fatalf("chunk size %d: capabilities frame not handled", size)
		}
		handler.Shutdown()
	}
}

// TestFirstFrameAcceptsEmptyCapabilities: a protocol 5 server without
// journal support advertises no capability, and its capabilities frame is
// still a valid first frame.
func TestFirstFrameAcceptsEmptyCapabilities(t *testing.T) {
	stream := append(capabilitiesFrame(), payloadFrames(protocol.EscapedControlFraming, "line\n")...)
	recorder := &payloadRecorder{}
	handler := NewClientHandler("srv1", recorder)
	defer handler.Shutdown()
	writeInChunks(t, handler, stream, 1)
	if handler.Status() > 0 {
		t.Fatalf("status = %d, want no error", handler.Status())
	}
	if !handler.WaitForCapabilities(0) || len(handler.Capabilities()) != 0 {
		t.Fatalf("capabilities = %q (arrived %v), want none arrived", handler.Capabilities(),
			handler.WaitForCapabilities(0))
	}
	if got := recorder.payload.String(); got != "line\n" {
		t.Fatalf("payload = %q, want %q", got, "line\n")
	}
}

// TestFirstFrameRejectsServerOfOlderProtocol: a first frame that is not the
// marked capabilities frame of a protocol 5 server sets status 1, also when a
// marked capabilities frame follows. An unmarked hidden first frame (an older
// server's capabilities frame) is not printed; any other one is.
func TestFirstFrameRejectsServerOfOlderProtocol(t *testing.T) {
	tests := []struct {
		name  string
		first []byte
		want  string
	}{
		{name: "unmarked capabilities frame",
			first: unmarkedCapabilitiesFrame(protocol.CapabilityJournalV1)},
		{name: "marked other control frame", first: controlFrames(".syn close soon")},
		{name: "payload first", first: payloadFrames(protocol.TerminalFraming, "version mismatch\n"),
			want: "version mismatch\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)
			defer handler.Shutdown()
			writeInChunks(t, handler, test.first, 1)
			if handler.Status() != 1 {
				t.Fatalf("status = %d, want 1", handler.Status())
			}
			// A later capabilities frame must not rehabilitate the session.
			writeInChunks(t, handler, capabilitiesFrame(protocol.CapabilityJournalV1), 1)
			if handler.Status() != 1 {
				t.Fatal("a late capabilities frame cleared the error")
			}
			if got := recorder.payload.String(); got != test.want {
				t.Fatalf("payload = %q, want %q", got, test.want)
			}
		})
	}
}

// TestHealthHandlerDecodesEscapedFrames: the health check session has no
// capabilities frame, so its first frame is not checked; its frames are
// escaped and marked like those of every other session.
func TestHealthHandlerDecodesEscapedFrames(t *testing.T) {
	tests := []struct {
		name       string
		message    protocol.Message
		wantStatus int
	}{
		{name: "server OK", message: protocol.Message{Kind: protocol.MessageServer, Hostname: "h", Content: "OK"},
			wantStatus: 0},
		{name: "server OK with newline", message: protocol.Message{Kind: protocol.MessageServer,
			Hostname: "h", Content: "OK\n"}, wantStatus: 0},
		{name: "server error", message: protocol.Message{Kind: protocol.MessageServer, Hostname: "h",
			Content: "ERROR|not ok"}, wantStatus: 2},
		{name: "plain NOT OK", message: protocol.Message{Kind: protocol.MessagePlain, Content: "NOT OK"},
			wantStatus: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			health := NewHealthHandler("srv1", nil)
			defer health.Shutdown()
			var stream bytes.Buffer
			protocol.EscapedControlFraming.EncodeMessage(&stream, test.message)
			protocol.EscapedControlFraming.EncodeControl(&stream,
				protocol.HiddenCommandFailedPrefix+protocol.CommandFailureNoFile)
			for _, b := range stream.Bytes() {
				if _, err := health.Write([]byte{b}); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
			}
			if health.Status() != test.wantStatus {
				t.Fatalf("status = %d, want %d", health.Status(), test.wantStatus)
			}
			if got := health.Outcome().Failures; !slices.Equal(got, []string{protocol.CommandFailureNoFile}) {
				t.Fatalf("Failures = %q, want the marked control frame handled", got)
			}
		})
	}
}

func TestMaprHandlerDecodesEscapedAggregateFrames(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	session := maprclient.NewSessionState(query, logging.NopLogger{})
	logger := &recordingClientLogger{}
	handler := NewMaprHandler("srv1", session, logger)

	serialized := strings.Join([]string{
		"5€ ¬ Ь",
		"1",
		query.Select[0].FieldStorage + protocol.AggregateKVDelimiter + "5€ ¬ Ь",
		handlerCountStorage(t, query) + protocol.AggregateKVDelimiter + "3",
		"",
	}, protocol.AggregateDelimiter)
	var aggregate bytes.Buffer
	protocol.EscapedControlFraming.EncodeMessage(&aggregate, protocol.Message{
		Kind:     protocol.MessageAggregate,
		Hostname: "host1",
		Content:  serialized + "\n",
	})
	stream := append(capabilitiesFrame(protocol.CapabilityJournalV1), aggregate.Bytes()...)
	if _, writeErr := handler.Write(stream); writeErr != nil {
		t.Fatalf("Write() error = %v", writeErr)
	}
	handler.Shutdown()

	if len(logger.errors) != 0 {
		t.Fatalf("unexpected client errors: %q", logger.errors)
	}
	result, numRows, err := session.Snapshot().GlobalGroup.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 || !strings.Contains(result, "5€ ¬ Ь") {
		t.Fatalf("aggregate result = %q (%d rows), want the group key 5€ ¬ Ь", result, numRows)
	}
}
