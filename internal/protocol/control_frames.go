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
// Under RawFraming and EscapedFraming a control frame is an untagged frame
// like a --plain log line, so the client can only tell them apart by their
// content: a log line starting with '.' or reading "AUTHKEY OK" is taken for
// a control frame (and dropped, or even acted upon, e.g. ".syn close
// connection"). EscapedControlFraming marks control frames instead: their
// body starts with FrameEscape and controlFrameCode. Escaping turns every
// FrameEscape of a body into FrameEscape escapedEscapeCode, so no escaped
// body starts with the marker, and every unmarked frame is payload.

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
// frame marker; otherwise it is an untagged frame (EncodeMessage with
// MessagePlain), as clients that do not mark control frames expect it.
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
// only finds control frames under EscapedControlFraming; under the other
// framings it returns frame and false, and the client has to tell control
// frames by their content as before.
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
