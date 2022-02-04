package logformat

// MakeFieldsGENERIC is the generic log line parser.
func (p *Parser) MakeFieldsGENERIC(maprLine string) (map[string]string, error) {
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
