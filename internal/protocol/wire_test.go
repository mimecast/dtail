package protocol

import (
	"bytes"
	"reflect"
	"testing"
)

func TestLineCodecRoundTrip(t *testing.T) {
	line := Line{
		Hostname:           "host-a",
		TransmittedPercent: "87",
		Number:             42,
		SourceID:           "app-worker.log",
		Content:            []byte("WARN|worker restarted\n"),
	}
	want := DecodedLine{
		Hostname:           line.Hostname,
		TransmittedPercent: line.TransmittedPercent,
		Number:             "42",
		SourceID:           line.SourceID,
		Content:            string(line.Content),
	}

	var encoded bytes.Buffer
	RawFraming.EncodeLine(&encoded, line)

	got, err := DecodeLine(encodedPayload(t, &encoded))
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded line = %#v, want %#v", got, want)
	}
}

func TestDecodeLineRejectsMalformedFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{name: "wrong type", frame: "SERVER|host|message"},
		{name: "missing content", frame: "REMOTE|host|100|1|source"},
		{name: "invalid line number", frame: "REMOTE|host|100|first|source|content"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeLine(test.frame); err == nil {
				t.Fatalf("DecodeLine(%q) succeeded", test.frame)
			}
		})
	}
}

func TestDecodeLinePreservesFieldSpellingAndUnframedTerminalBytes(t *testing.T) {
	for _, number := range []string{"0007", "+7"} {
		t.Run(number, func(t *testing.T) {
			payload := "REMOTE|host|100|" + number + "|source|content" +
				string([]byte{MessageDelimiter})

			got, err := DecodeLine(payload)
			if err != nil {
				t.Fatalf("DecodeLine: %v", err)
			}
			if got.Number != number {
				t.Fatalf("decoded number = %q, want original spelling %q", got.Number, number)
			}
			if got.Content != "content"+string([]byte{MessageDelimiter}) {
				t.Fatalf("decoded content = %q, want terminal payload byte preserved", got.Content)
			}
		})
	}
}

func TestMessageCodec(t *testing.T) {
	tests := []struct {
		name string
		want Message
	}{
		{
			name: "server message",
			want: Message{Kind: MessageServer, Hostname: "host-a", Content: "ERROR|disk full\n"},
		},
		{
			name: "aggregate message",
			want: Message{Kind: MessageAggregate, Hostname: "host-b", Content: "key|value"},
		},
		{
			name: "hidden message",
			want: Message{Kind: MessagePlain, Content: ".syn close connection"},
		},
		{
			name: "plain message",
			want: Message{Kind: MessagePlain, Content: "AUTHKEY OK\n"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var encoded bytes.Buffer
			RawFraming.EncodeMessage(&encoded, test.want)

			got, err := DecodeMessage(encodedPayload(t, &encoded))
			if err != nil {
				t.Fatalf("DecodeMessage: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decoded message = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestDecodeMessageRejectsMalformedKnownFrames(t *testing.T) {
	for _, frame := range []string{"SERVER", "SERVER|host", "AGGREGATE|host"} {
		t.Run(frame, func(t *testing.T) {
			if _, err := DecodeMessage(frame); err == nil {
				t.Fatalf("DecodeMessage(%q) succeeded", frame)
			}
		})
	}
}

func TestDecodeMessageRetainsKindForIncompleteTaggedFrame(t *testing.T) {
	got, err := DecodeMessage("AGGREGATE|host")
	if err == nil {
		t.Fatal("DecodeMessage succeeded")
	}
	if got.Kind != MessageAggregate || got.Hostname != "host" {
		t.Fatalf("decoded message = %#v, want aggregate kind and parsed hostname", got)
	}
}

// TestCutAggregateContentMatchesDecodeMessage checks that the client's
// byte-level aggregate classification agrees with DecodeMessage.
func TestCutAggregateContentMatchesDecodeMessage(t *testing.T) {
	for _, payload := range []string{
		"AGGREGATE|host|a∥1∥k≔v∥", "AGGREGATE|host|", "AGGREGATE|host|x|y", "AGGREGATE|host",
		"AGGREGATE|", "AGGREGATE", "AGGREGATEX|host|c", "SERVER|host|AGGREGATE|h|c", "", "AUTHKEY OK",
	} {
		t.Run(payload, func(t *testing.T) {
			checkCutAggregateContent(t, payload)
		})
	}
}

func FuzzCutAggregateContent(f *testing.F) {
	for _, seed := range []string{"AGGREGATE|h|c", "AGGREGATE|h", "AGGREGATE", "SERVER|h|c", "x"} {
		f.Add(seed)
	}
	f.Fuzz(checkCutAggregateContent)
}

func checkCutAggregateContent(t *testing.T, payload string) {
	decoded, decodeErr := DecodeMessage(payload)
	content, isAggregate, err := CutAggregateContent([]byte(payload))
	if isAggregate != (decoded.Kind == MessageAggregate) {
		t.Fatalf("CutAggregateContent(%q) isAggregate = %v, DecodeMessage kind %q", payload, isAggregate, decoded.Kind)
	}
	if !isAggregate {
		return
	}
	if (err == nil) != (decodeErr == nil) || (err != nil && err.Error() != decodeErr.Error()) {
		t.Fatalf("CutAggregateContent(%q) error = %v, DecodeMessage error %v", payload, err, decodeErr)
	}
	if err == nil && string(content) != decoded.Content {
		t.Fatalf("CutAggregateContent(%q) content = %q, want %q", payload, content, decoded.Content)
	}
}

func encodedPayload(t *testing.T, encoded *bytes.Buffer) string {
	t.Helper()
	frame := encoded.Bytes()
	if len(frame) == 0 || frame[len(frame)-1] != MessageDelimiter {
		t.Fatalf("encoded frame %q has no message delimiter", frame)
	}
	return string(frame[:len(frame)-1])
}
