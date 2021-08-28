package logformat

import (
	"errors"
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

// MakeFieldsDEFAULT is the default log file mapreduce parser.
func (p *Parser) MakeFieldsDEFAULT(maprLine string) (map[string]string, error) {
	fields := make(map[string]string, 20)
	splitted := strings.Split(maprLine, protocol.FieldDelimiter)

	fields["*"] = "*"
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$hostname"] = p.hostname
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	for _, kv := range splitted {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			return fields, errors.New("Error parsing mapr token: " + kv)
		}
		fields[strings.ToLower(keyAndValue[0])] = keyAndValue[1]
	}

	return fields, nil
}
