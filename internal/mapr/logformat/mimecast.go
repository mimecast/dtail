package logformat

import "errors"

// ErrMimecastNotAvailable is thrown in the open source version of DTail
var ErrMimecastNotAvailable error = errors.New("The mimecast logformat is not available in this build of DTail")

type mimecastParser struct{}

func newMimecastParser(hostname, timeZoneName string, timeZoneOffset int) (*mimecastParser, error) {
	return &mimecastParser{}, ErrMimecastNotAvailable
}

func (p *mimecastParser) MakeFields(maprLine string) (map[string]string, error) {
	return nil, ErrMimecastNotAvailable
}
