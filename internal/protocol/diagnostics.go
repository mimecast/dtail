package protocol

import (
	"fmt"
	"strings"
)

// Diagnostic is a hostname-tagged diagnostic emitted by code running in a
// client process. Details are encoded as separate protocol fields.
type Diagnostic struct {
	Source   string
	Hostname string
	Level    string
	Details  []any
}

// TimedDiagnostic is a local diagnostic emitted with its timestamp. Details
// are encoded as separate protocol fields.
type TimedDiagnostic struct {
	Level     string
	Timestamp string
	Details   []any
}

// DecodedDiagnostic is a hostname-tagged diagnostic whose remaining fields
// are retained verbatim in Content.
type DecodedDiagnostic struct {
	Source   string
	Hostname string
	Content  string
}

// EncodeDiagnostic encodes a hostname-tagged diagnostic without stream
// framing. It retains the historical trailing field separator when Details is
// empty.
func EncodeDiagnostic(diagnostic Diagnostic) string {
	var encoded strings.Builder
	AppendDiagnostic(&encoded, diagnostic)
	return encoded.String()
}

// AppendDiagnostic appends a hostname-tagged diagnostic to encoded.
func AppendDiagnostic(encoded *strings.Builder, diagnostic Diagnostic) {
	writeFields(encoded, diagnostic.Source, diagnostic.Hostname, diagnostic.Level)
	encoded.WriteString(fieldDelimiter)
	writeDiagnosticDetails(encoded, diagnostic.Details)
}

// EncodeTimedDiagnostic encodes a timestamped local diagnostic without stream
// framing. It retains the historical trailing field separator when Details is
// empty.
func EncodeTimedDiagnostic(diagnostic TimedDiagnostic) string {
	var encoded strings.Builder
	AppendTimedDiagnostic(&encoded, diagnostic)
	return encoded.String()
}

// AppendTimedDiagnostic appends a timestamped local diagnostic to encoded.
func AppendTimedDiagnostic(encoded *strings.Builder, diagnostic TimedDiagnostic) {
	writeFields(encoded, diagnostic.Level, diagnostic.Timestamp)
	encoded.WriteString(fieldDelimiter)
	writeDiagnosticDetails(encoded, diagnostic.Details)
}

// DecodeDiagnostic decodes the three visible parts of a hostname-tagged
// diagnostic. Content retains any additional field separators verbatim.
func DecodeDiagnostic(payload string) (DecodedDiagnostic, error) {
	fields := strings.SplitN(payload, fieldDelimiter, 3)
	if len(fields) != 3 {
		return DecodedDiagnostic{}, fmt.Errorf("decode diagnostic: malformed tagged frame")
	}
	return DecodedDiagnostic{
		Source:   fields[0],
		Hostname: fields[1],
		Content:  fields[2],
	}, nil
}

// EncodeServerError encodes a user-facing server error without stream framing.
func EncodeServerError(hostname, message string) string {
	return EncodeDiagnostic(Diagnostic{
		Source:   ServerMessageID,
		Hostname: hostname,
		Level:    "ERROR",
		Details:  []any{message},
	})
}

// EncodeDiagnosticDetails encodes diagnostic arguments without a header.
func EncodeDiagnosticDetails(details []any) string {
	var encoded strings.Builder
	AppendDiagnosticDetails(&encoded, details)
	return encoded.String()
}

// AppendDiagnosticDetails appends diagnostic arguments without a header.
func AppendDiagnosticDetails(encoded *strings.Builder, details []any) {
	writeDiagnosticDetails(encoded, details)
}

// EncodeStatsLine encodes client connection statistics without stream framing.
// Field order is intentionally unspecified, matching Go map iteration.
func EncodeStatsLine(fields map[string]any) string {
	var encoded strings.Builder
	i := 0
	for name, value := range fields {
		if i > 0 {
			encoded.WriteString(fieldDelimiter)
		}
		encoded.WriteString(name)
		encoded.WriteByte('=')
		fmt.Fprintf(&encoded, "%v", value)
		i++
	}
	return encoded.String()
}

// FieldSeparator returns the protocol field separator for presentation APIs
// that render separators independently from field content.
func FieldSeparator() string {
	return fieldDelimiter
}

// ScanField returns the next field in a delimiter-separated protocol payload.
func ScanField(payload string, start int) (field string, next int, done bool) {
	index := strings.IndexByte(payload[start:], fieldDelimiter[0])
	if index < 0 {
		return payload[start:], len(payload), true
	}
	index += start
	return payload[start:index], index + 1, false
}

func writeFields(dst *strings.Builder, fields ...string) {
	for i, field := range fields {
		if i > 0 {
			dst.WriteString(fieldDelimiter)
		}
		dst.WriteString(field)
	}
}

func writeDiagnosticDetails(dst *strings.Builder, details []any) {
	for i, detail := range details {
		if i > 0 {
			dst.WriteString(fieldDelimiter)
		}
		switch value := detail.(type) {
		case string:
			dst.WriteString(value)
		case error:
			dst.WriteString(value.Error())
		default:
			fmt.Fprintf(dst, "%v", value)
		}
	}
}
