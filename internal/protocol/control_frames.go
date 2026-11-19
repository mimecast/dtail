package protocol

import (
	"bytes"
	"strings"
)

// Control frames are the frames of the server-to-client stream that the
// client consumes itself instead of showing them: hidden messages starting
// with '.' (".syn capabilities ...", ".syn close connection", ".syn command
// failed ...", the session acks) and the AUTHKEY replies ("AUTHKEY OK",
// "AUTHKEY ERR ...").
//
// EscapedControlFraming, the framing of every session since protocol 5,
// marks control frames: their body starts with FrameEscape and
// controlFrameCode. Escaping turns every FrameEscape of a body into
// FrameEscape escapedEscapeCode, so no escaped body starts with the marker,
// and every unmarked frame is payload, whatever its content: a --plain log
// line starting with '.' or reading "AUTHKEY OK" is printed like any other.
// (Before protocol 5 sessions could also use unmarked framings, in which the
// client told control frames from payload by their content only.)

// AuthKeyReplyPrefix starts the server's replies to an AUTHKEY command
// ("AUTHKEY OK", "AUTHKEY ERR <detail>").
const AuthKeyReplyPrefix = "AUTHKEY "

// MarksControl reports whether f marks control frames, i.e. whether a frame
// without the marker is never a control frame.
func (f Framing) MarksControl() bool {
	return f == EscapedControlFraming
}

// EncodeControl appends a control frame whose body is message to dst.
// Under EscapedControlFraming the body is escaped and starts with the control
// frame marker; under TerminalFraming, which is never sent to a client, it is
// an untagged frame (EncodeMessage with MessagePlain).
func (f Framing) EncodeControl(dst *bytes.Buffer, message string) {
	if f.MarksControl() {
		dst.WriteByte(FrameEscape)
		dst.WriteByte(controlFrameCode)
	}
	f.EncodeMessage(dst, Message{Kind: MessagePlain, Content: message})
}

// CutControl reports whether frame, a received frame with its
// MessageDelimiter removed but not yet decoded (DecodeFrame), is a control
// frame of framing f, and returns it without the control frame marker. It
// only finds control frames under EscapedControlFraming; under
// TerminalFraming it returns frame and false.
func (f Framing) CutControl(frame []byte) ([]byte, bool) {
	if !f.MarksControl() || len(frame) < 2 || frame[0] != FrameEscape || frame[1] != controlFrameCode {
		return frame, false
	}
	return frame[2:], true
}

// IsControlMessage reports whether message, a message the server itself
// sends to the client (never payload), is a control message: a hidden
// message or an AUTHKEY reply.
func IsControlMessage(message string) bool {
	return IsHiddenMessage(message) || strings.HasPrefix(message, AuthKeyReplyPrefix)
}

// IsHiddenMessage reports whether message is a hidden control message, i.e.
// starts with '.'.
func IsHiddenMessage(message string) bool {
	return len(message) > 0 && message[0] == '.'
}
