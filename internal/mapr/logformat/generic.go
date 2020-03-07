package logformat

// MakeFieldsGENEROC is the generic log line parser.
func (p *Parser) MakeFieldsGENERIC(maprLine string) (map[string]string, error) {
	fields := make(map[string]string, 3)

	fields["*"] = "*"
	fields["$hostname"] = p.hostname
	fields["$line"] = maprLine
	fields["$empty"] = ""

	return fields, nil
}
