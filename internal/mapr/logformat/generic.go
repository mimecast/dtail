package logformat

import (
	"errors"

	"github.com/mimecast/dtail/internal/mapr"
)

// genericParser keeps its defaultParser in a named field instead of embedding
// it. Embedding would promote defaultParser.MakeFieldsInto onto this type, so
// overriding only MakeFields would leave the allocation-free path parsing
// lines in DTail's own MAPREDUCE layout (see the FieldsIntoParser doc
// comment). With a named field the compiler demands both methods here.
type genericParser struct {
	base defaultParser
}

var _ Parser = (*genericParser)(nil)
var _ FieldsIntoParser = (*genericParser)(nil)
var _ queryAwareParser = (*genericParser)(nil)

func newGenericParser(hostname, timeZoneName string, timeZoneOffset int) (*genericParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericParser{}, err
	}
	return &genericParser{base: *defaultParser}, nil
}

func (p *genericParser) setQuery(query *mapr.Query) {
	p.base.setQuery(query)
}

func (p *genericParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
	fields := make(map[string]string, p.base.fieldsCapacity)
	if err := p.MakeFieldsInto(fields, maprLine, sourceID); err != nil {
		// Currently unreachable: this parser's MakeFieldsInto ignores no line
		// and always returns nil. Kept for symmetry with default.go and csv.go
		// so the nil-map contract still holds if it ever starts ignoring lines.
		if errors.Is(err, ErrIgnoreFields) {
			return nil, err
		}
		return fields, err
	}
	return fields, nil
}

func (p *genericParser) MakeFieldsInto(dst map[string]string, maprLine, _ string) error {
	clear(dst)
	p.base.addDefaultFields(dst, maprLine)

	return nil
}
