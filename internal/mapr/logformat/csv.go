package logformat

import (
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

type csvParser struct {
	defaultParser
	header    []string
	hasHeader bool
}

func newCSVParser(hostname, timeZoneName string, timeZoneOffset int) (*csvParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &csvParser{}, err
	}
	return &csvParser{defaultParser: *defaultParser}, nil
}

func (p *csvParser) MakeFields(maprLine string) (map[string]string, error) {
	if !p.hasHeader {
		p.parseHeader(maprLine)
		return nil, ErrIgnoreFields
	}

	fields := make(map[string]string, 7+len(p.header))
	fields["*"] = "*"
	fields["$hostname"] = p.hostname
	fields["$server"] = p.hostname
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	splitted := strings.Split(maprLine, protocol.CSVDelimiter)
	for i, value := range splitted {
		if i >= len(p.header) {
			return fields, fmt.Errorf("CSV file seems corrupted, more fields than header values?")
		}
		fields[p.header[i]] = value
	}

	return fields, nil
}

func (p *csvParser) parseHeader(maprLine string) {
	p.header = strings.Split(maprLine, protocol.CSVDelimiter)
	p.hasHeader = true
}
