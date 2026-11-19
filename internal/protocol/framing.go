package protocol

import (
	"bytes"
	"unsafe"
)

// Framing selects how the body of a frame is written between two
// MessageDelimiter bytes on the server-to-client stream.
//
// MessageDelimiter is the single byte 0xAC. That byte is also a UTF-8
// continuation byte: it occurs in every character whose code point is 0x2C
// modulo 64, for example '¬' (C2 AC), '€' (E2 82 AC) and 'Ь' (D0 AC). Raw
// framing therefore cuts a log line containing such a character into two
// messages and loses the 0xAC byte. Escaped framing removes every 0xAC byte
// from frame bodies, so MessageDelimiter only ever terminates a frame.
//
// Which framing a session uses is negotiated per session so that clients and
// servers of different versions keep working together (see
// CapabilityEscapedFramingV1 and EscapedFramingCommand): only when the server
// advertises the capability and the client asks for it are frames escaped.
// EscapedControlFraming additionally marks control frames, so that no
// payload frame can be taken for one (see CapabilityControlFramesV1 and
// control_frames.go).
type Framing uint8

const (
	// RawFraming writes frame bodies verbatim. It is the legacy framing of
	// protocol 4.1 and stays in use with every peer that did not negotiate
	// EscapedFraming.
	RawFraming Framing = iota
	// EscapedFraming replaces each MessageDelimiter byte of a frame body with
	// FrameEscape followed by escapedDelimiterCode, and each FrameEscape byte
	// with FrameEscape followed by escapedEscapeCode. All other bytes, and so
	// all UTF-8 text without 0xAC, stay unchanged.
	EscapedFraming
	// EscapedControlFraming escapes every frame body like EscapedFraming and
	// starts the body of every control frame (hidden ".syn ..." messages and
	// AUTHKEY replies, see EncodeControl) with FrameEscape followed by
	// controlFrameCode. A body escaped this way never starts with that
	// sequence, so a frame without it is never a control frame, whatever its
	// content (e.g. a --plain log line ".syn close connection").
	EscapedControlFraming
)

const (
	// FrameEscape introduces an escape sequence in an EscapedFraming body.
	// 0xFF never occurs in valid UTF-8, so escaping it costs nothing for
	// UTF-8 logs and keeps the escaped body of ASCII frames byte-identical to
	// the raw one.
	FrameEscape byte = 0xFF
	// escapedDelimiterCode follows FrameEscape in place of a body byte equal
	// to MessageDelimiter.
	escapedDelimiterCode byte = 0x01
	// escapedEscapeCode follows FrameEscape in place of a body byte equal to
	// FrameEscape.
	escapedEscapeCode byte = 0x02
	// controlFrameCode follows FrameEscape at the very start of a control
	// frame body under EscapedControlFraming. It stands for no body byte.
	controlFrameCode byte = 0x03
	// shortBodyLen is the length up to which a body is scanned for bytes to
	// escape in one plain loop instead of with escapeScanner.
	shortBodyLen = 32
)

// Write appends body bytes of a frame to dst in framing f. It does not
// terminate the frame. Under EscapedFraming it takes time linear in
// len(body), whatever the share of bytes to escape (see writeEscaped).
func (f Framing) Write(dst *bytes.Buffer, body []byte) {
	if !f.escapes() {
		dst.Write(body)
		return
	}
	writeEscaped(dst, body)
}

// WriteString appends body bytes of a frame to dst in framing f. It does
// not terminate the frame.
func (f Framing) WriteString(dst *bytes.Buffer, body string) {
	if !f.escapes() {
		dst.WriteString(body)
		return
	}
	// writeEscaped only reads body and dst.Write copies what it is given, so
	// viewing the string's bytes without copying them is safe; it keeps a
	// single escaper for both body types.
	writeEscaped(dst, unsafe.Slice(unsafe.StringData(body), len(body)))
}

// EncodePayload appends an untagged frame whose body is payload, e.g. a
// plain-mode log line.
func (f Framing) EncodePayload(dst *bytes.Buffer, payload []byte) {
	f.Write(dst, payload)
	dst.WriteByte(MessageDelimiter)
}

// DecodeFrame returns the body of a frame received in framing f, with its
// MessageDelimiter already removed. When the frame holds no escape sequence
// (always under RawFraming) it is returned as it is, without copying, and
// frame is never modified; otherwise the decoded body is built in *scratch,
// which keeps the grown buffer for the next frame, and the result is only
// valid until scratch is used again. Malformed sequences (an unknown code or
// a FrameEscape at the end) are kept verbatim; no encoder produces them.
// Under EscapedControlFraming the control frame marker must be cut off first
// (CutControl): a decoded body may start with the same two bytes.
func (f Framing) DecodeFrame(frame []byte, scratch *[]byte) []byte {
	if !f.escapes() {
		return frame
	}
	i := bytes.IndexByte(frame, FrameEscape)
	if i < 0 {
		return frame
	}
	// Every iteration consumes the frame up to and including the escape
	// sequence it found, so decoding is linear in the frame length.
	decoded := (*scratch)[:0]
	for ; ; i = bytes.IndexByte(frame, FrameEscape) {
		if i < 0 || i+1 == len(frame) {
			decoded = append(decoded, frame...)
			break
		}
		decoded = append(decoded, frame[:i]...)
		switch frame[i+1] {
		case escapedDelimiterCode:
			decoded = append(decoded, MessageDelimiter)
		case escapedEscapeCode:
			decoded = append(decoded, FrameEscape)
		default:
			decoded = append(decoded, frame[i], frame[i+1])
		}
		frame = frame[i+2:]
	}
	*scratch = decoded
	return decoded
}

// String returns the name of the framing for diagnostics.
func (f Framing) String() string {
	switch f {
	case EscapedFraming:
		return "escaped"
	case EscapedControlFraming:
		return "escaped-control"
	default:
		return "raw"
	}
}

// escapes reports whether f escapes frame bodies.
func (f Framing) escapes() bool {
	return f == EscapedFraming || f == EscapedControlFraming
}

// writeEscaped appends body to dst in EscapedFraming. The escaped body is
// appended to the spare capacity of dst and committed with one Write, which
// is cheaper than a Write per run and escape sequence. A longer body is
// sized exactly first: two vectorised bytes.Count passes give the number of
// bytes to escape, so dst grows once to the escaped length and the escaped
// body never outgrows the spare capacity. A dense 1 MiB body thus costs one
// allocation of its escaped size (at most 2 MiB) in an empty dst. Growing by
// the raw length only, as before, made append reallocate several times and
// Write grow dst once more: 5 to 7 allocations of 6 to 11 MB in all. A body
// without bytes to escape costs the two Count passes and one copy, and never
// makes dst allocate more than a raw frame would. Short bodies such as header
// fields are scanned in one plain loop, which is cheaper there than the
// vectorised calls.
func writeEscaped(dst *bytes.Buffer, body []byte) {
	if len(body) <= shortBodyLen {
		dst.Grow(len(body))
		dst.Write(appendEscapedShort(dst.AvailableBuffer(), body))
		return
	}
	escapes := bytes.Count(body, delimiterByte) + bytes.Count(body, frameEscapeByte)
	if escapes == 0 {
		dst.Write(body)
		return
	}
	dst.Grow(len(body) + escapes)
	out := dst.AvailableBuffer()
	scanner := newEscapeScanner(body)
	start := 0
	for i := scanner.next(); i >= 0; i = scanner.next() {
		out = append(out, body[start:i]...)
		out = append(out, FrameEscape, escapeCode(body[i]))
		start = i + 1
	}
	// out still aliases the spare capacity of dst, so this Write only
	// commits the escaped bytes and does not allocate.
	dst.Write(append(out, body[start:]...))
}

// delimiterByte and frameEscapeByte are the single-byte separators
// writeEscaped counts with bytes.Count.
var (
	delimiterByte   = []byte{MessageDelimiter}
	frameEscapeByte = []byte{FrameEscape}
)

// appendEscapedShort appends body in EscapedFraming to out; it is the plain
// loop writeEscaped uses for bodies of at most shortBodyLen bytes.
func appendEscapedShort(out, body []byte) []byte {
	start := 0
	for i, b := range body {
		if b == MessageDelimiter || b == FrameEscape {
			out = append(out, body[start:i]...)
			out = append(out, FrameEscape, escapeCode(b))
			start = i + 1
		}
	}
	return append(out, body[start:]...)
}

// escapeScanner returns the indexes of the bytes of a body that must be
// escaped, in order, in time linear in the body length. It keeps the next
// MessageDelimiter and the next FrameEscape position apart and only searches
// again for the kind it just returned, from the byte after it, so every body
// byte is scanned at most once per kind. (Searching for the first byte of
// either kind afresh at every escape rescans the rest of the body for the
// other kind each time, which is quadratic on a body full of one kind: about
// 50 s for 1 MiB of 0xFF.)
type escapeScanner struct {
	body []byte
	// delimiter and escape are the indexes of the next MessageDelimiter and
	// the next FrameEscape not returned yet, or len(body) when none is left.
	delimiter int
	escape    int
}

func newEscapeScanner(body []byte) escapeScanner {
	return escapeScanner{
		body:      body,
		delimiter: indexByteFrom(body, 0, MessageDelimiter),
		escape:    indexByteFrom(body, 0, FrameEscape),
	}
}

// next returns the index of the next byte to escape, or -1 when none is
// left.
func (s *escapeScanner) next() int {
	i := min(s.delimiter, s.escape)
	switch {
	case i == len(s.body):
		return -1
	case i == s.delimiter:
		s.delimiter = indexByteFrom(s.body, i+1, MessageDelimiter)
	default:
		s.escape = indexByteFrom(s.body, i+1, FrameEscape)
	}
	return i
}

// indexByteFrom returns the index of the first c in body at or after from,
// or len(body) when there is none.
func indexByteFrom(body []byte, from int, c byte) int {
	if i := bytes.IndexByte(body[from:], c); i >= 0 {
		return from + i
	}
	return len(body)
}

// escapeCode returns the code that follows FrameEscape in place of a body
// byte that is MessageDelimiter or FrameEscape.
func escapeCode(b byte) byte {
	if b == MessageDelimiter {
		return escapedDelimiterCode
	}
	return escapedEscapeCode
}
