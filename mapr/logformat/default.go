package logformat

import (
	"errors"
	"strings"
)

// MakeFieldsDEFAULT is the default log file mapreduce parser.
func (p *Parser) MakeFieldsDEFAULT(maprLine string) (map[string]string, error) {
	fields := make(map[string]string, 20)
	splitted := strings.Split(maprLine, "|")

	fields["$hostname"] = p.hostname

	for _, kv := range splitted {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			return fields, errors.New("Error parsing mapr token: " + kv)
		}
		fields[strings.ToLower(keyAndValue[0])] = keyAndValue[1]
	}
	return fields, nil
}
