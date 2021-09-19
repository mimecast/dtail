package logformat

import (
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

// MakeFieldsDEFAULT is the default DTail log file key-value parser.
func (p *Parser) MakeFieldsDEFAULT(maprLine string) (map[string]string, error) {
	splitted := strings.Split(maprLine, protocol.FieldDelimiter)

	if len(splitted) < 3 || !strings.HasPrefix(splitted[3], "MAPREDUCE:") || !strings.HasPrefix(splitted[0], "INFO") {
		// Not a DTail mapreduce log line.
		return nil, IgnoreFieldsErr
	}

	fields := make(map[string]string, len(splitted)+8)

	fields["*"] = "*"
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$hostname"] = p.hostname
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	fields["$severity"] = splitted[0]
	fields["$loglevel"] = splitted[0]
	// TODO: Parse time like we do at Mimecast
	fields["$time"] = splitted[1]

	for _, kv := range splitted[4:] {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			return fields, fmt.Errorf("Unable to parse key-value token '%s'", kv)
		}
		fields[strings.ToLower(keyAndValue[0])] = keyAndValue[1]
	}

	return fields, nil
}
