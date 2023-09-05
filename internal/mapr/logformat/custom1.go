package logformat

import "errors"

var ErrCustom1NotImplemented error = errors.New("custom1 log format is not implemented")

// Template for creating a custom log format.
type custom1Parser struct{}

func newCustom1Parser(hostname, timeZoneName string, timeZoneOffset int) (*custom1Parser, error) {
	return &custom1Parser{}, ErrCustom1NotImplemented
}

func (p *custom1Parser) MakeFields(maprLine string) (map[string]string, error) {
	return nil, ErrCustom1NotImplemented
}
