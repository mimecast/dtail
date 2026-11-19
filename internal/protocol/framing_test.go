package protocol

import (
	"bytes"
	"math"
	"runtime"
	"testing"
	"time"
)

// utf8WithDelimiterByte holds characters whose UTF-8 encoding contains the
// MessageDelimiter byte 0xAC: '€' (E2 82 AC), 'Ь' (D0 AC) and '¬' (C2 AC).
const utf8WithDelimiterByte = "price 5€ ok, soft Ь sign, not ¬ sign"

func TestEscapedFramingRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty", body: nil},
		{name: "ascii", body: []byte("INFO|plain ascii line\n")},
		{name: "utf8 with delimiter byte", body: []byte(utf8WithDelimiterByte + "\n")},
		{name: "only delimiters", body: []byte{MessageDelimiter, MessageDelimiter}},
		{name: "escape byte", body: []byte{'a', FrameEscape, 'b'}},
		{name: "escape byte before codes", body: []byte{FrameEscape, escapedDelimiterCode,
			FrameEscape, escapedEscapeCode}},
		{name: "escape then delimiter", body: []byte{FrameEscape, MessageDelimiter}},
		{name: "trailing escape byte", body: []byte{'x', FrameEscape}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var encoded bytes.Buffer
			EscapedFraming.EncodePayload(&encoded, test.body)
			frame := encoded.Bytes()
			if i := bytes.IndexByte(frame, MessageDelimiter); i != len(frame)-1 {
				t.Fatalf("delimiter at %d in %q, want only the terminating one", i, frame)
			}

			var scratch []byte
			decoded := EscapedFraming.DecodeFrame(frame[:len(frame)-1], &scratch)
			if !bytes.Equal(decoded, test.body) {
				t.Fatalf("decoded = %q, want %q", decoded, test.body)
			}

			var fromString bytes.Buffer
			EscapedFraming.WriteString(&fromString, string(test.body))
			if !bytes.Equal(fromString.Bytes(), frame[:len(frame)-1]) {
				t.Fatalf("WriteString = %q, Write = %q", fromString.Bytes(), frame[:len(frame)-1])
			}
		})
	}
}

func TestEscapedFramingKeepsTextWithoutSpecialBytesUnchanged(t *testing.T) {
	body := []byte("Grüße, こんにちは, ASCII|fields|123\n")
	var raw, escaped bytes.Buffer
	RawFraming.EncodePayload(&raw, body)
	EscapedFraming.EncodePayload(&escaped, body)
	if !bytes.Equal(raw.Bytes(), escaped.Bytes()) {
		t.Fatalf("escaped frame %q differs from raw frame %q", escaped.Bytes(), raw.Bytes())
	}

	frame := escaped.Bytes()[:escaped.Len()-1]
	var scratch []byte
	decoded := EscapedFraming.DecodeFrame(frame, &scratch)
	if &decoded[0] != &frame[0] {
		t.Fatal("DecodeFrame copied a frame without escape sequences")
	}
	if scratch != nil {
		t.Fatal("DecodeFrame used scratch for a frame without escape sequences")
	}
}

func TestRawFramingIsLegacyWireFormat(t *testing.T) {
	body := []byte(utf8WithDelimiterByte)
	var encoded bytes.Buffer
	RawFraming.EncodePayload(&encoded, body)
	want := append(append([]byte(nil), body...), MessageDelimiter)
	if !bytes.Equal(encoded.Bytes(), want) {
		t.Fatalf("raw frame = %q, want payload verbatim plus delimiter %q", encoded.Bytes(), want)
	}

	// Raw decoding never interprets escape sequences, so a raw frame from a
	// peer without escaped framing is passed on byte for byte.
	frame := []byte{'a', FrameEscape, escapedDelimiterCode}
	var scratch []byte
	if decoded := RawFraming.DecodeFrame(frame, &scratch); !bytes.Equal(decoded, frame) {
		t.Fatalf("raw DecodeFrame = %q, want %q", decoded, frame)
	}
}

func TestEscapedFramingDecodeKeepsMalformedSequences(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "unknown code", frame: []byte{'a', FrameEscape, 'z', 'b'}},
		{name: "escape at end", frame: []byte{'a', FrameEscape}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var scratch []byte
			decoded := EscapedFraming.DecodeFrame(test.frame, &scratch)
			if !bytes.Equal(decoded, test.frame) {
				t.Fatalf("decoded = %q, want malformed frame kept verbatim %q", decoded, test.frame)
			}
		})
	}
}

func TestEscapedFramingDecodeDoesNotModifyFrame(t *testing.T) {
	var encoded bytes.Buffer
	EscapedFraming.EncodePayload(&encoded, []byte(utf8WithDelimiterByte))
	frame := encoded.Bytes()[:encoded.Len()-1]
	original := append([]byte(nil), frame...)

	var scratch []byte
	first := EscapedFraming.DecodeFrame(frame, &scratch)
	if string(first) != utf8WithDelimiterByte {
		t.Fatalf("decoded = %q, want %q", first, utf8WithDelimiterByte)
	}
	if !bytes.Equal(frame, original) {
		t.Fatalf("DecodeFrame modified its input: %q, want %q", frame, original)
	}
	if cap(scratch) == 0 {
		t.Fatal("DecodeFrame did not keep its scratch buffer for reuse")
	}
}

func TestEscapedFramingLineAndMessageFrames(t *testing.T) {
	var stream bytes.Buffer
	EscapedFraming.EncodeLine(&stream, Line{
		Hostname:           "host-a",
		TransmittedPercent: "100",
		Number:             7,
		SourceID:           "src€",
		Content:            []byte(utf8WithDelimiterByte + "\n"),
	})
	EscapedFraming.EncodeMessage(&stream, Message{
		Kind:     MessageAggregate,
		Hostname: "host-a",
		Content:  "group≔5€",
	})

	frames := bytes.Split(bytes.TrimSuffix(stream.Bytes(), []byte{MessageDelimiter}),
		[]byte{MessageDelimiter})
	if len(frames) != 2 {
		t.Fatalf("stream holds %d frames, want 2: %q", len(frames), stream.Bytes())
	}
	var scratch []byte
	line, err := DecodeLine(string(EscapedFraming.DecodeFrame(frames[0], &scratch)))
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if line.Content != utf8WithDelimiterByte+"\n" || line.SourceID != "src€" {
		t.Fatalf("decoded line = %#v", line)
	}
	message, err := DecodeMessage(string(EscapedFraming.DecodeFrame(frames[1], &scratch)))
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if message.Kind != MessageAggregate || message.Content != "group≔5€" {
		t.Fatalf("decoded message = %#v", message)
	}
}

func BenchmarkFramingEncodeLine(b *testing.B) {
	line := Line{
		Hostname:           "serv-01",
		TransmittedPercent: "100",
		Number:             123456,
		SourceID:           "app.log",
		Content:            bytes.Repeat([]byte("INFO|1002-071143|1|stats.go:56|8|15|7|0.21|"), 4),
	}
	for _, framing := range []Framing{RawFraming, EscapedFraming} {
		b.Run(framing.String(), func(b *testing.B) {
			var dst bytes.Buffer
			for i := 0; i < b.N; i++ {
				dst.Reset()
				framing.EncodeLine(&dst, line)
			}
		})
		b.Run("encoder/"+framing.String(), func(b *testing.B) {
			encoder := framing.NewLineEncoder(line.Hostname, line.TransmittedPercent)
			var dst bytes.Buffer
			for i := 0; i < b.N; i++ {
				dst.Reset()
				encoder.Encode(&dst, line.Number, line.SourceID, line.Content)
			}
		})
	}
}

func TestLineEncoderMatchesEncodeLine(t *testing.T) {
	lines := []Line{
		{Hostname: "host-a", TransmittedPercent: "100", Number: 1, SourceID: "a.log",
			Content: []byte("first\n")},
		{Hostname: "host-a", TransmittedPercent: "100", Number: 1234567890, SourceID: "a.log",
			Content: []byte(utf8WithDelimiterByte)},
		{Hostname: "host-a", TransmittedPercent: "100", Number: 3, SourceID: "b€.log",
			Content: []byte{FrameEscape, MessageDelimiter}},
		{Hostname: "host-a", TransmittedPercent: "100", Number: 0, SourceID: "",
			Content: nil},
		{Hostname: "host-a", TransmittedPercent: "100", Number: 5, SourceID: "a.log",
			Content: []byte("again a\n")},
	}
	for _, framing := range []Framing{RawFraming, EscapedFraming} {
		encoder := framing.NewLineEncoder("host-a", "100")
		for _, line := range lines {
			var want, got bytes.Buffer
			framing.EncodeLine(&want, line)
			encoder.Encode(&got, line.Number, line.SourceID, line.Content)
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("%s: encoder frame %q, EncodeLine frame %q", framing, got.Bytes(), want.Bytes())
			}
		}
	}
}

func TestLineEncoderDoesNotAllocate(t *testing.T) {
	encoder := EscapedFraming.NewLineEncoder("host-a", "100")
	content := []byte(utf8WithDelimiterByte + "\n")
	var dst bytes.Buffer
	dst.Grow(4096)
	encoder.Encode(&dst, 1, "a.log", content)
	allocs := testing.AllocsPerRun(1000, func() {
		dst.Reset()
		encoder.Encode(&dst, 123456789, "a.log", content)
	})
	if allocs != 0 {
		t.Fatalf("Encode allocated %v objects per line, want 0", allocs)
	}
}

func BenchmarkFramingEncodePayload(b *testing.B) {
	ascii := bytes.Repeat([]byte("INFO|1002-071143|1|stats.go:56|8|15|7|0.21|"), 4)
	utf8 := bytes.Repeat([]byte(utf8WithDelimiterByte), 4)
	for _, bench := range []struct {
		name    string
		framing Framing
		body    []byte
	}{
		{"raw/ascii", RawFraming, ascii},
		{"escaped/ascii", EscapedFraming, ascii},
		{"escaped/utf8", EscapedFraming, utf8},
	} {
		b.Run(bench.name, func(b *testing.B) {
			var dst bytes.Buffer
			b.SetBytes(int64(len(bench.body)))
			for i := 0; i < b.N; i++ {
				dst.Reset()
				bench.framing.EncodePayload(&dst, bench.body)
			}
		})
	}
}

// referenceEscape is the byte-by-byte definition of EscapedFraming bodies
// that the optimised escaper must match.
func referenceEscape(body []byte) []byte {
	out := make([]byte, 0, 2*len(body))
	for _, b := range body {
		switch b {
		case MessageDelimiter:
			out = append(out, FrameEscape, escapedDelimiterCode)
		case FrameEscape:
			out = append(out, FrameEscape, escapedEscapeCode)
		default:
			out = append(out, b)
		}
	}
	return out
}

// denseEscapeBodies returns bodies of size bytes made only of bytes to
// escape, all 0xFF and all 0xAC, and a pseudo-random mix of both with plain
// bytes.
func denseEscapeBodies(size int) map[string][]byte {
	mixed := make([]byte, size)
	state := uint32(1)
	for i := range mixed {
		state = state*1664525 + 1013904223
		switch state >> 30 {
		case 0:
			mixed[i] = MessageDelimiter
		case 1:
			mixed[i] = FrameEscape
		default:
			mixed[i] = 'a'
		}
	}
	return map[string][]byte{
		"only 0xFF": bytes.Repeat([]byte{FrameEscape}, size),
		"only 0xAC": bytes.Repeat([]byte{MessageDelimiter}, size),
		"mixed":     mixed,
	}
}

// TestEscapedFramingDenseBodies checks escaping and decoding of 1 MiB bodies
// made only of bytes to escape.
func TestEscapedFramingDenseBodies(t *testing.T) {
	for name, body := range denseEscapeBodies(1 << 20) {
		t.Run(name, func(t *testing.T) {
			want := referenceEscape(body)
			var fromBytes, fromString bytes.Buffer
			EscapedFraming.Write(&fromBytes, body)
			EscapedFraming.WriteString(&fromString, string(body))
			if !bytes.Equal(fromBytes.Bytes(), want) || !bytes.Equal(fromString.Bytes(), want) {
				t.Fatal("escaped body differs from the reference escaping")
			}
			var scratch []byte
			if decoded := EscapedFraming.DecodeFrame(fromBytes.Bytes(), &scratch); !bytes.Equal(decoded, body) {
				t.Fatal("decoded body differs from the original")
			}
		})
	}
}

// TestEscapedFramingDenseBodiesAreLinear guards against quadratic escaping:
// an escaper that searched the rest of the body for both bytes at every
// escape took about 50 s for 1 MiB of 0xFF. A body 16 times as long must
// take about 16 times as long to escape and decode, not 256 times; the bound
// of 64 times leaves room for a loaded machine and the race detector, which
// make absolute bounds unreliable.
func TestEscapedFramingDenseBodiesAreLinear(t *testing.T) {
	const small, large = 64 << 10, 1 << 20
	smallBodies, largeBodies := denseEscapeBodies(small), denseEscapeBodies(large)
	for name := range largeBodies {
		t.Run(name, func(t *testing.T) {
			smallTime := max(fastestEscapeAndDecode(smallBodies[name], 3), time.Millisecond)
			largeTime := fastestEscapeAndDecode(largeBodies[name], 3)
			if largeTime > 64*smallTime {
				t.Fatalf("1 MiB took %v, 64 KiB %v: escaping grows faster than linearly",
					largeTime, smallTime)
			}
		})
	}
}

// fastestEscapeAndDecode returns the fastest of runs escapes and decodes of
// body.
func fastestEscapeAndDecode(body []byte, runs int) time.Duration {
	fastest := time.Duration(math.MaxInt64)
	var dst bytes.Buffer
	var scratch []byte
	for range runs {
		dst.Reset()
		start := time.Now()
		EscapedFraming.Write(&dst, body)
		EscapedFraming.DecodeFrame(dst.Bytes(), &scratch)
		fastest = min(fastest, time.Since(start))
	}
	return fastest
}

// TestEscapedFramingMatchesReference compares the escaper with the reference
// on pseudo-random bodies of many lengths around shortBodyLen, written into
// buffers with and without spare capacity and after existing content.
func TestEscapedFramingMatchesReference(t *testing.T) {
	alphabet := []byte{MessageDelimiter, FrameEscape, 'a', 0xE2, 0x82, escapedDelimiterCode}
	state := uint32(7)
	for length := 0; length <= 4*shortBodyLen; length++ {
		body := make([]byte, length)
		for i := range body {
			state = state*1664525 + 1013904223
			body[i] = alphabet[int(state>>16)%len(alphabet)]
		}
		want := append([]byte("prefix"), referenceEscape(body)...)
		for _, spare := range []int{0, length / 2, 2 * length} {
			dst := bytes.NewBuffer(make([]byte, 0, len("prefix")+spare))
			dst.WriteString("prefix")
			EscapedFraming.Write(dst, body)
			if !bytes.Equal(dst.Bytes(), want) {
				t.Fatalf("length %d spare %d: escaped %q, want %q", length, spare, dst.Bytes(), want)
			}
		}
	}
}

func BenchmarkFramingEncodeDensePayload(b *testing.B) {
	for name, body := range denseEscapeBodies(1 << 20) {
		b.Run(name, func(b *testing.B) {
			var dst bytes.Buffer
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				dst.Reset()
				EscapedFraming.EncodePayload(&dst, body)
			}
		})
	}
}

// TestEscapedFramingSizesDenseBodiesExactly: escaping a long body into an
// empty buffer allocates its storage once, of the escaped size, however many
// of its bytes are escaped. An escaper that grew dst by the raw length and
// then appended past it reallocated while appending and grew dst again on
// Write: 5 to 7 allocations of about 6 to 11 MB in all for these bodies.
func TestEscapedFramingSizesDenseBodiesExactly(t *testing.T) {
	for name, body := range denseEscapeBodies(1 << 20) {
		t.Run(name, func(t *testing.T) {
			escapedLen := len(referenceEscape(body))
			dst := new(bytes.Buffer)
			EscapedFraming.Write(dst, body)
			if dst.Len() != escapedLen {
				t.Fatalf("escaped %d bytes, want %d", dst.Len(), escapedLen)
			}
			// bytes.Buffer rounds a grow up only a little (to its size class),
			// so the storage stays close to the escaped size.
			if dst.Cap() > escapedLen+escapedLen/8 {
				t.Errorf("buffer capacity %d for %d escaped bytes", dst.Cap(), escapedLen)
			}
			// One allocation for the buffer header and one for its storage,
			// which the race detector makes bytes.Buffer allocate twice.
			maxAllocs, maxBytes := 2.0, uint64(escapedLen+escapedLen/8)
			if raceEnabled {
				maxAllocs, maxBytes = 3, 2*maxBytes
			}
			allocs := testing.AllocsPerRun(1, func() { EscapedFraming.Write(new(bytes.Buffer), body) })
			if allocs > maxAllocs {
				t.Errorf("escaping 1 MiB allocated %v objects, want at most %v", allocs, maxAllocs)
			}
			allocated := allocatedBytes(func() { EscapedFraming.Write(new(bytes.Buffer), body) })
			if allocated > maxBytes {
				t.Errorf("escaping 1 MiB allocated %d bytes, want at most %d for %d escaped bytes",
					allocated, maxBytes, escapedLen)
			}
		})
	}
}

// allocatedBytes returns the bytes the heap allocated while running f.
func allocatedBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
