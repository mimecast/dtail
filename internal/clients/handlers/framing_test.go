package handlers

import (
	"bytes"
	"encoding/base64"
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

func capabilitiesFrame(capabilities ...string) []byte {
	var stream bytes.Buffer
	protocol.RawFraming.EncodeMessage(&stream, protocol.Message{
		Kind:    protocol.MessagePlain,
		Content: protocol.HiddenCapabilitiesPrefix + strings.Join(capabilities, " "),
	})
	return stream.Bytes()
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

// TestClientHandlersRequestEscapedFramingFirst: the escaped framing request
// is the first command frame and the control frames request the second, both
// in the same Read, ahead of every other command.
func TestClientHandlersRequestEscapedFramingFirst(t *testing.T) {
	want := "protocol " + protocol.ProtocolCompat + " base64 " +
		base64.StdEncoding.EncodeToString([]byte(protocol.EscapedFramingCommand)) + ";" +
		"protocol " + protocol.ProtocolCompat + " base64 " +
		base64.StdEncoding.EncodeToString([]byte(protocol.ControlFramesCommand)) + ";"

	client := NewClientHandler("srv1", nil)
	go func() { _ = client.SendMessage("AUTHKEY abc") }()
	if got := readFirstFrame(t, &client.baseHandler); got != want {
		t.Fatalf("first client frames = %q, want the framing requests %q", got, want)
	}
	if got := readFirstFrame(t, &client.baseHandler); !strings.Contains(got, "base64") {
		t.Fatalf("second client frame = %q, want the AUTHKEY command", got)
	}

	query, err := mapr.NewQuery("select count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	mapClient := NewMaprHandler("srv1", maprclient.NewSessionState(query, logging.NopLogger{}), nil)
	if got := readFirstFrame(t, &mapClient.baseHandler); got != want {
		t.Fatalf("first mapr frames = %q, want the framing requests %q", got, want)
	}
}

// TestHealthHandlerDoesNotRequestEscapedFraming: the server's health handler
// ends the session after the first command, so a framing request must not
// precede the health command.
func TestHealthHandlerDoesNotRequestEscapedFraming(t *testing.T) {
	health := NewHealthHandler("srv1", nil)
	if health.framingRequested || len(health.pendingCommand) != 0 {
		t.Fatalf("health handler requested escaped framing: pending %q", health.pendingCommand)
	}
	if _, err := health.Write(capabilitiesFrame(protocol.CapabilityEscapedFramingV1)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if health.framing() != protocol.RawFraming {
		t.Fatal("health handler switched to escaped framing without requesting it")
	}
}

func TestClientHandlerDecodesFramesPerNegotiatedFraming(t *testing.T) {
	escapedLines := payloadFrames(protocol.EscapedFraming, framingTestLines...)
	tests := []struct {
		name         string
		request      bool
		capabilities []string
		payload      []byte
		want         string
	}{
		{
			name: "requested and advertised", request: true,
			capabilities: []string{protocol.CapabilityQueryUpdateV1, protocol.CapabilityEscapedFramingV1},
			payload:      escapedLines,
			want:         strings.Join(framingTestLines, ""),
		},
		{
			// An older server: raw frames cut lines at 0xAC, as before.
			name: "requested but not advertised", request: true,
			capabilities: []string{protocol.CapabilityQueryUpdateV1},
			payload:      payloadFrames(protocol.RawFraming, "price 5\xe2\x82", " ok\n"),
			want:         "price 5\xe2\x82\n ok\n",
		},
		{
			// Never decoded as escaped unless requested: the escape
			// sequences reach the output verbatim.
			name: "advertised but not requested", request: false,
			capabilities: []string{protocol.CapabilityEscapedFramingV1},
			payload:      payloadFrames(protocol.RawFraming, "a\xff\x01b\n"),
			want:         "a\xff\x01b\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)
			handler.framingRequested = test.request
			stream := append(capabilitiesFrame(test.capabilities...), test.payload...)
			for _, size := range []int{1, 3, len(stream)} {
				recorder.payload.Reset()
				handler.framingState.Store(uint32(protocol.RawFraming))
				handler.framingDecided = false
				writeInChunks(t, handler, stream, size)
				if got := recorder.payload.String(); got != test.want {
					t.Fatalf("chunk size %d: payload = %q, want %q", size, got, test.want)
				}
			}
		})
	}
}

// TestClientHandlerDecidesFramingFromFirstFrameOnly: only the session's first
// frame decides the framing. A capabilities frame listing escaped framing
// that arrives later, e.g. a log line of a raw --plain session from an older
// server that reads like one, leaves the session raw.
func TestClientHandlerDecidesFramingFromFirstFrameOnly(t *testing.T) {
	lateCapabilities := capabilitiesFrame(protocol.CapabilityEscapedFramingV1)
	tests := []struct {
		name  string
		first []byte
	}{
		{name: "payload first", first: payloadFrames(protocol.RawFraming, "early \xff\x01\n")},
		{name: "capabilities without escaped framing first",
			first: capabilitiesFrame(protocol.CapabilityQueryUpdateV1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)
			var stream []byte
			stream = append(stream, test.first...)
			stream = append(stream, lateCapabilities...)
			stream = append(stream, payloadFrames(protocol.RawFraming, "late \xff\x01 raw\n")...)
			if _, err := handler.Write(stream); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if handler.framing() != protocol.RawFraming {
				t.Fatal("a capabilities frame after the first frame switched the framing")
			}
			if got := recorder.payload.String(); !strings.HasSuffix(got, "late \xff\x01 raw\n") {
				t.Fatalf("payload = %q, want the late frame kept raw", got)
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
	protocol.EscapedFraming.EncodeMessage(&aggregate, protocol.Message{
		Kind:     protocol.MessageAggregate,
		Hostname: "host1",
		Content:  serialized + "\n",
	})
	stream := append(capabilitiesFrame(protocol.CapabilityEscapedFramingV1), aggregate.Bytes()...)
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
