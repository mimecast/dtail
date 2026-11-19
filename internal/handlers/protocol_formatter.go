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

// newLineFormatter returns the formatter of one line output format. framing
// applies to the frame-producing formats (protocol and delimited) only: the
// newline and colored formats write terminal output, not frames.
func newLineFormatter(format lineFormat, hostname string, colorizer Colorizer,
	framing protocol.Framing) lineFormatter {
	switch format {
	case lineFormatDelimited:
		return func(dst *bytes.Buffer, content []byte, _ uint64, _ string) {
			framing.EncodePayload(dst, content)
		}
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
		// One encoder per formatter, i.e. per line writer, whose mutex
		// serializes the calls; it escapes the constant header fields once.
		encoder := framing.NewLineEncoder(hostname, defaultTransmittedPerc)
		return func(dst *bytes.Buffer, content []byte, lineNumber uint64, sourceID string) {
			encoder.Encode(dst, lineNumber, sourceID, content)
		}
	}
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

	// The colored line is terminal output, not a frame: RawFraming only
	// borrows the frame layout, whose delimiter is cut off again below.
	var encoded bytes.Buffer
	protocol.RawFraming.EncodeLine(&encoded, protocol.Line{
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
