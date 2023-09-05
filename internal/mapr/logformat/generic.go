package logformat

type genericParser struct {
	defaultParser
}

func newGenericParser(hostname, timeZoneName string, timeZoneOffset int) (*genericParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericParser{}, err
	}
	return &genericParser{defaultParser: *defaultParser}, nil
}

func (p *genericParser) MakeFields(maprLine string) (map[string]string, error) {
	fields := make(map[string]string, 3)

	fields["*"] = "*"
	fields["$hostname"] = p.hostname
	fields["$server"] = p.hostname
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	return fields, nil
}
