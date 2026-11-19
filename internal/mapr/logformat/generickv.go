package logformat

import (
	"errors"

	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

// genericKVParser keeps its defaultParser in a named field rather than
// embedding it, for the reason spelled out on genericParser and in the
// FieldsIntoParser doc comment: a promoted MakeFieldsInto would silently parse
// these lines in DTail's own MAPREDUCE layout.
type genericKVParser struct {
	base defaultParser
}

var _ Parser = (*genericKVParser)(nil)
var _ FieldsIntoParser = (*genericKVParser)(nil)
var _ queryAwareParser = (*genericKVParser)(nil)

func newGenericKVParser(hostname, timeZoneName string, timeZoneOffset int) (*genericKVParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericKVParser{}, err
	}
	return &genericKVParser{base: *defaultParser}, nil
}

func (p *genericKVParser) setQuery(query *mapr.Query) {
	p.base.setQuery(query)
}

func (p *genericKVParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
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

func (p *genericKVParser) MakeFieldsInto(dst map[string]string, maprLine, _ string) error {
	clear(dst)
	p.base.addDefaultFields(dst, maprLine)
	start := 0

	for {
		token, next, done := protocol.ScanField(maprLine, start)
		// Generic key-value logs may mix structured and unstructured fields.
		// Ignore malformed fields while continuing to parse later tokens.
		_ = p.base.addKeyValueField(dst, token)
		if done {
			break
		}
		start = next
	}

	return nil
}
