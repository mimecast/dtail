package logformat

type genericParser struct {
	defaultParser
}

var _ Parser = (*genericParser)(nil)

func newGenericParser(hostname, timeZoneName string, timeZoneOffset int) (*genericParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericParser{}, err
	}
	return &genericParser{defaultParser: *defaultParser}, nil
}

func (p *genericParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	fields := make(map[string]string, p.fieldsCapacity)
	p.addDefaultFields(fields, maprLine)

	return fields, nil
}
