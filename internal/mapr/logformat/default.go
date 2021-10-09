package logformat

import (
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

// MakeFieldsDEFAULT is the default DTail log file key-value parser.
func (p *Parser) MakeFieldsDEFAULT(maprLine string) (map[string]string, error) {
	splitted := strings.Split(maprLine, protocol.FieldDelimiter)

	if len(splitted) < 11 || !strings.HasPrefix(splitted[9], "MAPREDUCE:") ||
		!strings.HasPrefix(splitted[0], "INFO") {
		// Not a DTail mapreduce log line.
		return nil, ErrIgnoreFields
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

	time := splitted[1]
	fields["$time"] = time
	if len(time) == 15 {
		// Example: 20211002-071209
		fields["$date"] = time[0:8]
		fields["$hour"] = time[9:11]
		fields["$minute"] = time[11:13]
		fields["$second"] = time[13:]
	}
	fields["$pid"] = splitted[2]
	fields["$caller"] = splitted[3]
	fields["$cpus"] = splitted[4]
	fields["$goroutines"] = splitted[5]
	fields["$cgocalls"] = splitted[6]
	fields["$loadavg"] = splitted[7]
	fields["$uptime"] = splitted[8]

	for _, kv := range splitted[10:] {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			return fields, fmt.Errorf("Unable to parse key-value token '%s'", kv)
		}
		fields[strings.ToLower(keyAndValue[0])] = keyAndValue[1]
	}

	return fields, nil
}
