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
// MessageDelimiter. Number retains its original spelling so presentation
// code does not rewrite compatible frames such as a zero-padded line number.
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

// EncodeLine appends a complete remote log-line frame to dst. Header fields
// must not contain FieldDelimiter; the compatibility wire format has no escape
// sequence for delimiters in those fields.
func EncodeLine(dst *bytes.Buffer, line Line) {
	dst.WriteString(LineMessageID)
	dst.WriteString(FieldDelimiter)
	dst.WriteString(line.Hostname)
	dst.WriteString(FieldDelimiter)
	dst.WriteString(line.TransmittedPercent)
	dst.WriteString(FieldDelimiter)
	dst.WriteString(strconv.FormatUint(line.Number, 10))
	dst.WriteString(FieldDelimiter)
	dst.WriteString(line.SourceID)
	dst.WriteString(FieldDelimiter)
	dst.Write(line.Content)
	dst.WriteByte(MessageDelimiter)
}

// DecodeLine decodes a remote log-line payload after its stream-level
// MessageDelimiter has been removed.
func DecodeLine(payload string) (DecodedLine, error) {
	parts := strings.SplitN(payload, FieldDelimiter, 6)
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
// frame to dst. Hostname must not contain FieldDelimiter for tagged messages;
// the compatibility wire format has no escape sequence for that field.
// Untagged messages retain their payload verbatim.
func EncodeMessage(dst *bytes.Buffer, message Message) {
	if message.Kind != MessagePlain {
		dst.WriteString(string(message.Kind))
		dst.WriteString(FieldDelimiter)
		dst.WriteString(message.Hostname)
		dst.WriteString(FieldDelimiter)
	}
	dst.WriteString(message.Content)
	dst.WriteByte(MessageDelimiter)
}

// DecodeMessage decodes a server or aggregate message. Unknown, hidden, and
// plain payloads are returned as MessagePlain so callers can preserve their
// existing routing. The stream-level MessageDelimiter must already have been
// removed.
func DecodeMessage(payload string) (Message, error) {
	parts := strings.SplitN(payload, FieldDelimiter, 3)
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
