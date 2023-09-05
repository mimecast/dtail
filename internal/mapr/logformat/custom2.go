package logformat

import "errors"

var ErrCustom2NotImplemented error = errors.New("custom2 log format is not implemented")

// Template for creating a custom log format.
type custom2Parser struct{}

func newCustom2Parser(hostname, timeZoneName string, timeZoneOffset int) (*custom2Parser, error) {
	return &custom2Parser{}, ErrCustom2NotImplemented
}

func (p *custom2Parser) MakeFields(maprLine string) (map[string]string, error) {
	return nil, ErrCustom2NotImplemented
}
