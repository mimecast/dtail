package handlers

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/protocol"
)

// payloadRecorder is a client logger that records every payload write and
// whether it arrived through the byte-slice or the string path. It copies the
// bytes it receives, as a real sink does, so tests notice a handler that
// hands out a slice it later changes.
type payloadRecorder struct {
	clientlog.NopLogger
	payload    bytes.Buffer
	writes     []string
	stringRaws int
	warnings   []string
	debugs     []string
}

func (r *payloadRecorder) RawBytes(message []byte) {
	r.payload.Write(message)
	r.writes = append(r.writes, string(message))
}

func (r *payloadRecorder) Raw(message string) string {
	r.stringRaws++
	r.payload.WriteString(message)
	r.writes = append(r.writes, message)
	return message
}

func (r *payloadRecorder) Warn(args ...any) string {
	message := fmt.Sprint(args...)
	r.warnings = append(r.warnings, message)
	return message
}

func (r *payloadRecorder) Debug(args ...any) string {
	message := fmt.Sprint(args...)
	r.debugs = append(r.debugs, message)
	return message
}

// legacyReceiver runs the receive loop baseHandler used before commit
// 35a3bc4, copied from that revision with only the receiver type changed: a
// byte-by-byte Write into receiveBuf and a string-based handleMessage. It
// calls the same hidden-message and AUTHKEY handlers as the current code, so
// comparing the two exercises exactly the part that was rewritten: framing,
// routing and newline handling.
type legacyReceiver struct {
	*baseHandler
}

func (h legacyReceiver) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n':
			// Just add the newline to the buffer, don't treat as message delimiter
			h.receiveBuf.WriteByte(b)
		case protocol.MessageDelimiter:
			message := h.receiveBuf.String()
			h.handleMessage(message)
			h.receiveBuf.Reset()
		default:
			h.receiveBuf.WriteByte(b)
		}
	}
	return len(p), nil
}

func (h legacyReceiver) handleMessage(message string) {
	if len(message) > 0 && message[0] == '.' {
		h.handleHiddenMessage(message)
		return
	}
	if h.handleAuthKeyMessage(message) {
		return
	}

	// Add newline only if the message doesn't already end with one
	if len(message) > 0 && message[len(message)-1] == '\n' {
		clientlog.Raw(h.log(), message)
	} else {
		clientlog.Raw(h.log(), message+"\n")
	}
}

// receiveObservation is everything a receive loop can do in response to a
// stream: payload sink calls (one entry per call, so a line split from its
// newline is visible), AUTHKEY warnings and debug messages, capabilities and
// session acknowledgements.
type receiveObservation struct {
	writes       []string
	warnings     []string
	debugs       []string
	capabilities []string
	acks         []SessionAck
}

func observe(handler *baseHandler, recorder *payloadRecorder) receiveObservation {
	var acks []SessionAck
	for {
		ack, ok := handler.WaitForSessionAck(0)
		if !ok {
			break
		}
		acks = append(acks, ack)
	}
	return receiveObservation{
		writes:       recorder.writes,
		warnings:     recorder.warnings,
		debugs:       recorder.debugs,
		capabilities: handler.Capabilities(),
		acks:         acks,
	}
}

func (o receiveObservation) diff(want receiveObservation) string {
	switch {
	case len(o.writes) != len(want.writes):
		return fmt.Sprintf("payload sink calls = %d, want %d", len(o.writes), len(want.writes))
	case !slices.Equal(o.writes, want.writes):
		for i := range o.writes {
			if o.writes[i] != want.writes[i] {
				return fmt.Sprintf("payload sink call %d differs: got %d bytes %.60q, want %d bytes %.60q",
					i, len(o.writes[i]), o.writes[i], len(want.writes[i]), want.writes[i])
			}
		}
	case !slices.Equal(o.warnings, want.warnings):
		return fmt.Sprintf("warnings = %q, want %q", o.warnings, want.warnings)
	case !slices.Equal(o.debugs, want.debugs):
		return fmt.Sprintf("debugs = %q, want %q", o.debugs, want.debugs)
	case !slices.Equal(o.capabilities, want.capabilities):
		return fmt.Sprintf("capabilities = %q, want %q", o.capabilities, want.capabilities)
	case !slices.Equal(o.acks, want.acks):
		return fmt.Sprintf("session acks = %+v, want %+v", o.acks, want.acks)
	}
	return ""
}

func frame(messages ...string) []byte {
	var stream []byte
	for _, message := range messages {
		stream = append(stream, message...)
		stream = append(stream, protocol.MessageDelimiter)
	}
	return stream
}

func writeInChunks(t *testing.T, handler *ClientHandler, stream []byte, size int) {
	t.Helper()
	for start := 0; start < len(stream); start += size {
		end := min(start+size, len(stream))
		n, err := handler.Write(stream[start:end])
		if err != nil || n != end-start {
			t.Fatalf("Write() = %d, %v; want %d, nil", n, err, end-start)
		}
	}
}

// TestBaseHandlerWriteMatchesLegacyLoop feeds a mixed stream to the current
// receive loop in chunks of many sizes, so delimiters land at every buffer
// position (first byte, last byte, split between chunks), and to the copy of
// the legacy byte-by-byte loop. Both must produce the same payload sink calls
// byte for byte, the same AUTHKEY warnings and debug output, and the same
// capability and session state. The stream mixes payload with and without a
// trailing newline, empty and newline-only messages, lines larger than bufio,
// io.Copy and maxRetainedLineBufBytes, hidden control messages, AUTHKEY
// acknowledgements (bare, padded and SERVER-wrapped), their look-alikes, and an
// unterminated tail that neither loop may print.
func TestBaseHandlerWriteMatchesLegacyLoop(t *testing.T) {
	longLine := strings.Repeat("0123456789abcdef", 12*1024) // 192 KiB
	stream := frame(
		"first line\n",
		"no trailing newline",
		"",
		"\n",
		"\n\n",
		".syn capabilities query-update-v1 journal-v1",
		"multi\nline\nmessage",
		"AUTHKEY OK",
		longLine,
		"SERVER|srv1|AUTHKEY ERR invalid base64\n",
		longLine+"\n",
		protocol.HiddenSessionStartOKPrefix+" 7",
		"  AUTHKEY OK \n",
		"REMOTE|host|100|1|src|content with | pipes\n",
		protocol.HiddenSessionErrorPrefix+" not supported",
		"AUTHKEY ERR",
		"AUTHKEYS are not acknowledgements",
		" .not hidden because of the leading space",
		"REMOTE|host|100|1|src|AUTHKEY OK\n",
		"SERVER|srv1|some server info\n",
		".unknown hidden message",
		"last\n",
	)
	stream = append(stream, "unterminated tail"...)

	legacyRecorder := &payloadRecorder{}
	legacy := NewClientHandler("srv1", legacyRecorder)
	if _, err := (legacyReceiver{&legacy.baseHandler}).Write(stream); err != nil {
		t.Fatalf("legacy Write() error = %v", err)
	}
	want := observe(&legacy.baseHandler, legacyRecorder)
	if len(want.writes) == 0 || len(want.debugs) == 0 || len(want.warnings) == 0 ||
		len(want.capabilities) == 0 || len(want.acks) != 2 {
		t.Fatalf("stream does not exercise every route of the legacy loop: %+v", want)
	}

	for _, size := range []int{1, 2, 3, 7, 13, 31, 4096, 32 * 1024, len(stream)} {
		t.Run(fmt.Sprintf("chunk%d", size), func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)

			writeInChunks(t, handler, stream, size)

			if diff := observe(&handler.baseHandler, recorder).diff(want); diff != "" {
				t.Fatalf("current loop differs from legacy loop: %s", diff)
			}
			if recorder.stringRaws != 0 {
				t.Fatalf("payload used the string Raw path %d times, want the byte path", recorder.stringRaws)
			}
		})
	}
}

// TestBaseHandlerWriteKeepsUnterminatedTail checks that a truncated message is
// never printed on its own and is completed by the next chunk.
func TestBaseHandlerWriteKeepsUnterminatedTail(t *testing.T) {
	recorder := &payloadRecorder{}
	handler := NewClientHandler("srv1", recorder)

	writeInChunks(t, handler, []byte("partial"), 64)
	if recorder.payload.Len() != 0 {
		t.Fatalf("unterminated message was printed: %q", recorder.payload.String())
	}

	chunk := append([]byte(" rest"), protocol.MessageDelimiter)
	chunk = append(chunk, "tail"...)
	writeInChunks(t, handler, chunk, 64)
	if got, want := recorder.payload.String(), "partial rest\n"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}

	writeInChunks(t, handler, frame("\n"), 64)
	if got, want := recorder.payload.String(), "partial rest\ntail\n"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

// TestBaseHandlerWriteConsumesControlMessages checks that hidden messages and
// AUTHKEY acknowledgements never reach the payload sink, while look-alikes do.
func TestBaseHandlerWriteConsumesControlMessages(t *testing.T) {
	stream := frame(
		".syn capabilities query-update-v1",
		"AUTHKEY OK",
		"SERVER|srv1|AUTHKEY ERR invalid base64\n",
		"  AUTHKEY OK \n",
		"payload\n",
		"SERVER|srv1|some server info\n",
		"AUTHKEYS are not acknowledgements",
		" .not hidden because of the leading space",
		"REMOTE|host|100|1|src|AUTHKEY OK\n",
	)
	want := "payload\n" +
		"SERVER|srv1|some server info\n" +
		"AUTHKEYS are not acknowledgements\n" +
		" .not hidden because of the leading space\n" +
		"REMOTE|host|100|1|src|AUTHKEY OK\n"

	for _, size := range []int{1, 5, len(stream)} {
		t.Run(fmt.Sprintf("chunk%d", size), func(t *testing.T) {
			recorder := &payloadRecorder{}
			handler := NewClientHandler("srv1", recorder)

			writeInChunks(t, handler, stream, size)

			if got := recorder.payload.String(); got != want {
				t.Fatalf("payload = %q, want %q", got, want)
			}
			if !handler.HasCapability(protocol.CapabilityQueryUpdateV1) {
				t.Fatalf("hidden capabilities message was not handled")
			}
			if len(recorder.warnings) != 1 || !strings.Contains(recorder.warnings[0], "invalid base64") {
				t.Fatalf("warnings = %q, want one AUTHKEY failure with its detail", recorder.warnings)
			}
		})
	}
}

// TestBaseHandlerWriteDoesNotModifyInput guards the io.Writer contract: the
// newline for a message without one must not be written into p.
func TestBaseHandlerWriteDoesNotModifyInput(t *testing.T) {
	handler := NewClientHandler("srv1", &payloadRecorder{})
	input := frame("no newline", "second", "unterminated")
	input = input[:len(input)-1]
	original := bytes.Clone(input)

	if _, err := handler.Write(input); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !bytes.Equal(input, original) {
		t.Fatalf("Write modified its input:\ngot  %q\nwant %q", input, original)
	}
}

// TestBaseHandlerWritePayloadDoesNotAllocate checks the steady state of the
// hot path: complete payload lines, with and without a trailing newline and
// with a message split across chunks, cost no heap allocations.
func TestBaseHandlerWritePayloadDoesNotAllocate(t *testing.T) {
	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	stream := frame("with newline\n", "without newline", "REMOTE|host|100|1|src|line\n")
	stream = append(stream, "split across"...)
	next := frame(" chunks\n")

	write := func() {
		_, _ = handler.Write(stream)
		_, _ = handler.Write(next)
	}
	write() // grow receiveBuf and lineBuf once

	if allocs := testing.AllocsPerRun(100, write); allocs != 0 {
		t.Fatalf("Write allocated %.1f times per run, want 0", allocs)
	}
}

// TestBaseHandlerWriteReleasesLargeLineBuffer checks that a huge message
// without a trailing newline does not leave its scratch buffer pinned to the
// connection, while ordinary lines keep reusing lineBuf without allocating.
func TestBaseHandlerWriteReleasesLargeLineBuffer(t *testing.T) {
	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	small := frame("an ordinary log line without newline")

	_, _ = handler.Write(small)
	smallBuf := handler.lineBuf
	if cap(smallBuf) == 0 || cap(smallBuf) > maxRetainedLineBufBytes {
		t.Fatalf("lineBuf cap after a small message = %d, want 1..%d", cap(smallBuf), maxRetainedLineBufBytes)
	}

	_, _ = handler.Write(frame(strings.Repeat("x", 4*maxRetainedLineBufBytes)))
	if cap(handler.lineBuf) > maxRetainedLineBufBytes {
		t.Fatalf("lineBuf cap after a large message = %d, want at most %d", cap(handler.lineBuf), maxRetainedLineBufBytes)
	}

	// A message just below the limit still fits into a retained buffer.
	_, _ = handler.Write(frame(strings.Repeat("y", maxRetainedLineBufBytes/2)))
	if cap(handler.lineBuf) == 0 {
		t.Fatalf("lineBuf was released after a message below the limit")
	}

	_, _ = handler.Write(small)
	if allocs := testing.AllocsPerRun(100, func() { _, _ = handler.Write(small) }); allocs != 0 {
		t.Fatalf("small messages after a released buffer allocated %.1f times per run, want 0", allocs)
	}
}

// TestMaprHandlerWritePrintsNonAggregatePayload locks in the mapr handler's
// newline handling after it switched to the byte-slice message path.
func TestMaprHandlerWritePrintsNonAggregatePayload(t *testing.T) {
	recorder := &payloadRecorder{}
	handler := &MaprHandler{baseHandler: baseHandler{server: "srv1", logger: recorder}}

	if _, err := handler.Write(frame("SERVER|srv1|hello\n", "plain", "a\nb", "AUTHKEY OK")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got, want := recorder.payload.String(), "SERVER|srv1|hello\nplain\nab\n"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestMayBeAuthKeyMessage(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{message: "AUTHKEY OK", want: true},
		{message: " \tAUTHKEY ERR detail\n", want: true},
		{message: "SERVER|host|AUTHKEY OK\n", want: true},
		{message: "AGGREGATE|host|AUTHKEY OK", want: true},
		{message: "", want: false},
		{message: "\n", want: false},
		{message: "2026-01-01T00:00:00Z INFO request=000000001 status=200\n", want: false},
		{message: "REMOTE|host|100|1|src|AUTHKEY OK\n", want: false},
		{message: "SERVER", want: false},
		{message: "AUTHKE", want: false},
	}
	for _, tc := range tests {
		if got := mayBeAuthKeyMessage([]byte(tc.message)); got != tc.want {
			t.Errorf("mayBeAuthKeyMessage(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}

// FuzzMayBeAuthKeyMessage proves the pre-check is safe: it may let through
// messages that are not acknowledgements, but it must never reject one that
// parseAuthKeyMessage accepts. The seeds include Unicode white space, which
// TrimSpace removes on both sides of the comparison.
func FuzzMayBeAuthKeyMessage(f *testing.F) {
	for _, seed := range []string{
		"AUTHKEY OK", "AUTHKEY ERR x", "SERVER|h|AUTHKEY OK", "AGGREGATE|h| AUTHKEY ERR\n",
		" AUTHKEY OK ", "SERVER|AUTHKEY OK", "SERVER||AUTHKEY OK", "payload\n", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, message string) {
		isAuthKey, _, _ := parseAuthKeyMessage(message)
		if isAuthKey && !mayBeAuthKeyMessage([]byte(message)) {
			t.Fatalf("pre-check rejected authkey message %q", message)
		}
	})
}

func BenchmarkBaseHandlerWrite(b *testing.B) {
	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	line := "2026-01-01T00:00:00Z INFO request=000000001 user001 path=/api/item/0001 status=200 payload=abcdefghijklmnopqrstuvwxyz0123456789\n"
	var chunk []byte
	for len(chunk) < 32*1024 {
		chunk = append(chunk, line...)
		chunk = append(chunk, protocol.MessageDelimiter)
	}
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = handler.Write(chunk)
	}
}
