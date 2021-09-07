package logformat

import (
	"errors"
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

// MakeFieldsDEFAULT is the default log file mapreduce parser.
func (p *Parser) MakeFieldsDEFAULT(maprLine string) (map[string]string, error) {
	splitted := strings.Split(maprLine, protocol.FieldDelimiter)
	fields := make(map[string]string, len(splitted))

	fields["*"] = "*"
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$hostname"] = p.hostname
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	kvStart := 0
	// DTail mapreduce format
	if len(splitted) > 3 && strings.HasPrefix(splitted[3], "MAPREDUCE:") {
		fields["$severity"] = splitted[0]
		// TODO: Parse time like we do at Mimecast
		fields["$time"] = splitted[1]
		kvStart = 4
	}

	for _, kv := range splitted[kvStart:] {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			return fields, errors.New("Error parsing mapreduce token: " + kv)
		}
		fields[strings.ToLower(keyAndValue[0])] = keyAndValue[1]
	}

	return fields, nil
}
