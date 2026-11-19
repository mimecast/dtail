package handlers

import (
	"bytes"

	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/protocol"
)

const defaultTransmittedPerc = "100"

type lineFormat uint8
type lineFormatter func(*bytes.Buffer, []byte, uint64, string)

// Colorizer renders a protocol frame for terminal output.
type Colorizer interface {
	Colorfy(string) string
}

type defaultColorizer struct{}

const (
	lineFormatProtocol lineFormat = iota
	lineFormatDelimited
	lineFormatNewline
	lineFormatColored
)

func newLineFormatter(format lineFormat, hostname string, colorizer Colorizer) lineFormatter {
	switch format {
	case lineFormatDelimited:
		return appendDelimitedLine
	case lineFormatNewline:
		return appendNewlineLine
	case lineFormatColored:
		if colorizer == nil {
			colorizer = defaultColorizer{}
		}
		return func(dst *bytes.Buffer, content []byte, lineNumber uint64, sourceID string) {
			appendColoredLine(dst, hostname, content, lineNumber, sourceID, colorizer)
		}
	default:
		return func(dst *bytes.Buffer, content []byte, lineNumber uint64, sourceID string) {
			protocol.EncodeLine(dst, protocol.Line{
				Hostname:           hostname,
				TransmittedPercent: defaultTransmittedPerc,
				Number:             lineNumber,
				SourceID:           sourceID,
				Content:            content,
			})
		}
	}
}

func appendDelimitedLine(dst *bytes.Buffer, content []byte, _ uint64, _ string) {
	dst.Write(content)
	dst.WriteByte(protocol.MessageDelimiter)
}

func appendNewlineLine(dst *bytes.Buffer, content []byte, _ uint64, _ string) {
	dst.Write(content)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		dst.WriteByte('\n')
	}
}

func appendColoredLine(dst *bytes.Buffer, hostname string, content []byte, lineNumber uint64,
	sourceID string, colorizer Colorizer) {
	if len(content) > 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}

	var encoded bytes.Buffer
	protocol.EncodeLine(&encoded, protocol.Line{
		Hostname:           hostname,
		TransmittedPercent: defaultTransmittedPerc,
		Number:             lineNumber,
		SourceID:           sourceID,
		Content:            content,
	})
	frame := encoded.Bytes()
	frame = frame[:len(frame)-1]
	dst.WriteString(colorizer.Colorfy(string(frame)))
	dst.WriteByte('\n')
}

func (defaultColorizer) Colorfy(line string) string {
	return brush.Default().Colorfy(line)
}
