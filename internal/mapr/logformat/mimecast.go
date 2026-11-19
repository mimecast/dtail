//go:build !proprietary

package logformat

import "errors"

// ErrMimecastNotAvailable is thrown in the open source version of DTail
var ErrMimecastNotAvailable error = errors.New("the mimecast logformat is not available in this build of DTail")

type mimecastParser struct{}

var _ Parser = (*mimecastParser)(nil)

func newMimecastParser(hostname, timeZoneName string, timeZoneOffset int) (*mimecastParser, error) {
	return &mimecastParser{}, ErrMimecastNotAvailable
}

func newMimecastGenericParser(hostname, timeZoneName string, timeZoneOffset int) (*mimecastParser, error) {
	return &mimecastParser{}, ErrMimecastNotAvailable
}

func (p *mimecastParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	return nil, ErrMimecastNotAvailable
}
