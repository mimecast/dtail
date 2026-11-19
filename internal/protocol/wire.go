package protocol

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const (
	// LineMessageID identifies a remote log-line message.
	LineMessageID = "REMOTE"
	// ServerMessageID identifies a server diagnostic or control message.
	ServerMessageID = "SERVER"
)

// MessageKind identifies a protocol message payload.
type MessageKind string

const (
	// MessagePlain identifies an untagged payload, including hidden control messages.
	MessagePlain MessageKind = ""
	// MessageServer identifies a server diagnostic or control message.
	MessageServer MessageKind = ServerMessageID
	// MessageAggregate identifies a MapReduce aggregate message.
	MessageAggregate MessageKind = MessageKind(AggregateMessageID)
)

// Line is a decoded remote log-line message.
type Line struct {
	Hostname           string
	TransmittedPercent string
	Number             uint64
	SourceID           string
	Content            []byte
}

// DecodedLine is a remote log-line payload after stream framing has removed
// MessageDelimiter (and, under EscapedFraming, its escape sequences). Number
// retains its original spelling so presentation code does not rewrite
// compatible frames such as a zero-padded line number.
type DecodedLine struct {
	Hostname           string
	TransmittedPercent string
	Number             string
	SourceID           string
	Content            string
}

// Message is a decoded server, aggregate, or untagged message.
type Message struct {
	Kind     MessageKind
	Hostname string
	Content  string
}

// EncodeLine appends a complete remote log-line frame in framing f to dst.
// Header fields must not contain the field separator; the wire format has no
// escape sequence for field separators in those fields. Under EscapedFraming
// (and EscapedControlFraming) the whole frame body, header fields and
// content alike, is escaped, so a MessageDelimiter byte in any of them no
// longer ends the frame early.
func (f Framing) EncodeLine(dst *bytes.Buffer, line Line) {
	f.writeLineHeader(dst, line.Hostname, line.TransmittedPercent)
	writeLineNumber(dst, line.Number)
	f.WriteString(dst, line.SourceID)
	dst.WriteString(fieldDelimiter)
	f.Write(dst, line.Content)
	dst.WriteByte(MessageDelimiter)
}

// LineEncoder appends the log-line frames of one host in one framing; it is
// the per-line hot path of server-mode output. It writes the same bytes as
// Framing.EncodeLine, but escapes the header fields that are the same for
// every line (hostname and transmitted percent) once, and the source ID only
// when it changes. It is not safe for concurrent use; a line writer
// serializes its lines anyway.
type LineEncoder struct {
	framing Framing
	// prefix is "REMOTE|hostname|percent|" in framing.
	prefix []byte
	// sourceID and encodedSourceID cache the last source ID and its
	// encoding; cached is false until the first line.
	sourceID        string
	encodedSourceID []byte
	cached          bool
}

// NewLineEncoder returns a LineEncoder for the lines of hostname.
func (f Framing) NewLineEncoder(hostname, transmittedPercent string) *LineEncoder {
	var prefix bytes.Buffer
	f.writeLineHeader(&prefix, hostname, transmittedPercent)
	return &LineEncoder{framing: f, prefix: prefix.Bytes()}
}

// Encode appends the complete frame of one log line to dst.
func (e *LineEncoder) Encode(dst *bytes.Buffer, number uint64, sourceID string, content []byte) {
	dst.Write(e.prefix)
	writeLineNumber(dst, number)
	if !e.cached || sourceID != e.sourceID {
		var encoded bytes.Buffer
		e.framing.WriteString(&encoded, sourceID)
		e.sourceID, e.encodedSourceID, e.cached = sourceID, encoded.Bytes(), true
	}
	dst.Write(e.encodedSourceID)
	dst.WriteString(fieldDelimiter)
	e.framing.Write(dst, content)
	dst.WriteByte(MessageDelimiter)
}

// writeLineHeader appends "REMOTE|hostname|percent|" in framing f.
func (f Framing) writeLineHeader(dst *bytes.Buffer, hostname, transmittedPercent string) {
	dst.WriteString(LineMessageID)
	dst.WriteString(fieldDelimiter)
	f.WriteString(dst, hostname)
	dst.WriteString(fieldDelimiter)
	f.WriteString(dst, transmittedPercent)
	dst.WriteString(fieldDelimiter)
}

// writeLineNumber appends the decimal line number and its field separator,
// formatted on the stack so the hot path does not allocate.
func writeLineNumber(dst *bytes.Buffer, number uint64) {
	var digits [20]byte
	dst.Write(strconv.AppendUint(digits[:0], number, 10))
	dst.WriteString(fieldDelimiter)
}

// DecodeLine decodes a remote log-line payload after its stream-level
// MessageDelimiter has been removed and the frame was decoded with
// Framing.DecodeFrame.
func DecodeLine(payload string) (DecodedLine, error) {
	parts := strings.SplitN(payload, fieldDelimiter, 6)
	if len(parts) != 6 || parts[0] != LineMessageID {
		return DecodedLine{}, fmt.Errorf("decode line: malformed %s frame", LineMessageID)
	}

	number := strings.TrimPrefix(parts[3], "+")
	if number == "" {
		return DecodedLine{}, fmt.Errorf("decode line number %q: empty number", parts[3])
	}
	if _, err := strconv.ParseUint(number, 10, 64); err != nil {
		return DecodedLine{}, fmt.Errorf("decode line number %q: %w", parts[3], err)
	}

	return DecodedLine{
		Hostname:           parts[1],
		TransmittedPercent: parts[2],
		Number:             parts[3],
		SourceID:           parts[4],
		Content:            parts[5],
	}, nil
}

// EncodeMessage appends a complete server, aggregate, or untagged message
// frame in framing f to dst. Hostname must not contain the field separator
// for tagged messages; the wire format has no escape sequence for field
// separators in that field. Untagged messages retain their payload verbatim
// apart from the escaping of framing f.
func (f Framing) EncodeMessage(dst *bytes.Buffer, message Message) {
	if message.Kind != MessagePlain {
		dst.WriteString(string(message.Kind))
		dst.WriteString(fieldDelimiter)
		f.WriteString(dst, message.Hostname)
		dst.WriteString(fieldDelimiter)
	}
	f.WriteString(dst, message.Content)
	dst.WriteByte(MessageDelimiter)
}

// DecodeMessage decodes a server or aggregate message. Unknown, hidden, and
// plain payloads are returned as MessagePlain so callers can preserve their
// existing routing. The stream-level MessageDelimiter must already have been
// removed and the frame decoded with Framing.DecodeFrame.
func DecodeMessage(payload string) (Message, error) {
	parts := strings.SplitN(payload, fieldDelimiter, 3)
	if len(parts) == 0 {
		return Message{Kind: MessagePlain}, nil
	}

	kind := MessageKind(parts[0])
	switch kind {
	case MessageServer, MessageAggregate:
		if len(parts) != 3 {
			message := Message{}
			if len(parts) == 2 {
				message.Kind = kind
				message.Hostname = parts[1]
			}
			return message, fmt.Errorf("decode message: malformed %s frame", parts[0])
		}
		return Message{
			Kind:     kind,
			Hostname: parts[1],
			Content:  parts[2],
		}, nil
	default:
		return Message{Kind: MessagePlain, Content: payload}, nil
	}
}

// aggregateTag is the leading field of an aggregate message with its
// separator.
var aggregateTag = []byte(AggregateMessageID + fieldDelimiter)

// CutAggregateContent tells whether payload is an aggregate message and
// returns its content, for the client's per-message receive path: it
// classifies payload exactly like DecodeMessage, but without converting it to
// a string or splitting it. content aliases payload. isAggregate is false for
// any other message; err is set (and content nil) for an aggregate message
// that lacks the hostname field's separator, which DecodeMessage rejects as
// malformed with the same error.
func CutAggregateContent(payload []byte) (content []byte, isAggregate bool, err error) {
	rest, ok := bytes.CutPrefix(payload, aggregateTag)
	if !ok {
		return nil, false, nil
	}
	// fieldDelimiter is a single byte.
	hostnameEnd := bytes.IndexByte(rest, fieldDelimiter[0])
	if hostnameEnd < 0 {
		return nil, true, fmt.Errorf("decode message: malformed %s frame", AggregateMessageID)
	}
	return rest[hostnameEnd+1:], true, nil
}
