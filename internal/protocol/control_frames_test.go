package protocol

import (
	"bytes"
	"testing"
)

func TestEncodeControlMarksOnlyUnderEscapedControlFraming(t *testing.T) {
	const message = ".syn command failed 5€\n"
	for _, framing := range []Framing{RawFraming, EscapedFraming} {
		var control, plain bytes.Buffer
		framing.EncodeControl(&control, message)
		framing.EncodeMessage(&plain, Message{Kind: MessagePlain, Content: message})
		if !bytes.Equal(control.Bytes(), plain.Bytes()) {
			t.Fatalf("%v: control frame %q differs from the untagged frame %q",
				framing, control.Bytes(), plain.Bytes())
		}
		if _, ok := framing.CutControl(control.Bytes()[:control.Len()-1]); ok {
			t.Fatalf("%v: CutControl found a control frame", framing)
		}
	}

	var encoded, escaped bytes.Buffer
	EscapedControlFraming.EncodeControl(&encoded, message)
	EscapedFraming.EncodePayload(&escaped, []byte(message))
	want := append([]byte{FrameEscape, controlFrameCode}, escaped.Bytes()...)
	if !bytes.Equal(encoded.Bytes(), want) {
		t.Fatalf("control frame = %q, want %q", encoded.Bytes(), want)
	}
	frame := encoded.Bytes()[:encoded.Len()-1]
	if i := bytes.IndexByte(frame, MessageDelimiter); i >= 0 {
		t.Fatalf("delimiter at %d inside the control frame %q", i, frame)
	}
	body, ok := EscapedControlFraming.CutControl(frame)
	if !ok {
		t.Fatal("CutControl did not find the control frame")
	}
	var scratch []byte
	if got := EscapedControlFraming.DecodeFrame(body, &scratch); string(got) != message {
		t.Fatalf("decoded control frame = %q, want %q", got, message)
	}
}

// TestEscapedControlFramingPayloadNeverReadsAsControl: no payload, not even
// one starting with the marker bytes themselves, is encoded as a frame that
// CutControl takes for a control frame, and every payload decodes back to
// itself.
func TestEscapedControlFramingPayloadNeverReadsAsControl(t *testing.T) {
	payloads := [][]byte{
		nil,
		[]byte(".syn close connection\n"),
		[]byte("AUTHKEY OK\n"),
		{FrameEscape, controlFrameCode, '.', 's'},
		{FrameEscape},
		{FrameEscape, FrameEscape, controlFrameCode},
		{MessageDelimiter, FrameEscape, controlFrameCode},
	}
	for _, payload := range payloads {
		var encoded bytes.Buffer
		EscapedControlFraming.EncodePayload(&encoded, payload)
		frame := encoded.Bytes()[:encoded.Len()-1]
		body, ok := EscapedControlFraming.CutControl(frame)
		if ok {
			t.Fatalf("payload %q encoded as control frame %q", payload, frame)
		}
		var scratch []byte
		if got := EscapedControlFraming.DecodeFrame(body, &scratch); !bytes.Equal(got, payload) {
			t.Fatalf("payload %q decoded as %q", payload, got)
		}
	}
}

// TestEscapedControlFramingFramesLikeEscapedFraming: apart from control
// frames, EscapedControlFraming writes the same bytes as EscapedFraming.
func TestEscapedControlFramingFramesLikeEscapedFraming(t *testing.T) {
	line := Line{Hostname: "h€", TransmittedPercent: "100", Number: 7, SourceID: "s", Content: []byte("a\xac\xff\n")}
	message := Message{Kind: MessageServer, Hostname: "h", Content: "5€"}
	var escaped, control bytes.Buffer
	for _, out := range []struct {
		framing Framing
		dst     *bytes.Buffer
	}{{EscapedFraming, &escaped}, {EscapedControlFraming, &control}} {
		out.framing.EncodeLine(out.dst, line)
		out.framing.NewLineEncoder(line.Hostname, line.TransmittedPercent).Encode(out.dst,
			line.Number, line.SourceID, line.Content)
		out.framing.EncodeMessage(out.dst, message)
		out.framing.EncodePayload(out.dst, line.Content)
	}
	if !bytes.Equal(escaped.Bytes(), control.Bytes()) {
		t.Fatalf("escaped-control frames %q differ from escaped frames %q", control.Bytes(), escaped.Bytes())
	}
}

func TestIsControlMessage(t *testing.T) {
	tests := map[string]bool{
		".syn close connection": true,
		".":                     true,
		"AUTHKEY OK\n":          true,
		"AUTHKEY ERR detail\n":  true,
		"AUTHKEYS":              false,
		"":                      false,
		"ERROR|something":       false,
		" .syn":                 false,
	}
	for message, want := range tests {
		if got := IsControlMessage(message); got != want {
			t.Errorf("IsControlMessage(%q) = %v, want %v", message, got, want)
		}
	}
}

func TestFramingString(t *testing.T) {
	for framing, want := range map[Framing]string{RawFraming: "raw", EscapedFraming: "escaped",
		EscapedControlFraming: "escaped-control"} {
		if got := framing.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", framing, got, want)
		}
	}
}
