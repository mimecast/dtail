package logformat

import "errors"

// ErrCustom1NotImplemented indicates custom1 parser is only a template.
var ErrCustom1NotImplemented error = errors.New("custom1 log format is not implemented")

// Template for creating a custom log format.
type custom1Parser struct{}

var _ Parser = (*custom1Parser)(nil)

func newCustom1Parser(hostname, timeZoneName string, timeZoneOffset int) (*custom1Parser, error) {
	return &custom1Parser{}, ErrCustom1NotImplemented
}

func (p *custom1Parser) MakeFields(maprLine, _ string) (map[string]string, error) {
	return nil, ErrCustom1NotImplemented
}
