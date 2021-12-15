package logformat

import (
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

// MakeFieldsGENERIGKV is the generic key-value logfile parser.
func (p *Parser) MakeFieldsGENERIGKV(maprLine string) (map[string]string, error) {
	splitted := strings.Split(maprLine, protocol.FieldDelimiter)
	fields := make(map[string]string, len(splitted))

	fields["*"] = "*"
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$hostname"] = p.hostname
	fields["$server"] = p.hostname
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	for _, kv := range splitted[0:] {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			//dlog.Common.Debug("Unable to parse key-value token, ignoring it", kv)
			continue
		}
		fields[strings.ToLower(keyAndValue[0])] = keyAndValue[1]
	}

	return fields, nil
}
