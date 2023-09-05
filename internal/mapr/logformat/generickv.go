package logformat

import (
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

type genericKVParser struct {
	defaultParser
}

func newGenericKVParser(hostname, timeZoneName string, timeZoneOffset int) (*genericKVParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericKVParser{}, err
	}
	return &genericKVParser{defaultParser: *defaultParser}, nil
}

func (p *genericKVParser) MakeFields(maprLine string) (map[string]string, error) {
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
		fields[keyAndValue[0]] = keyAndValue[1]
	}

	return fields, nil
}
