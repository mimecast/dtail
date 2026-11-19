package logformat

import (
	"fmt"
	"sync"

	"github.com/mimecast/dtail/internal/protocol"
)

// csvParser parses CSV log lines. The first line encountered for a given
// sourceID is treated as the column header and stored so that subsequent
// lines from the same source can be mapped to named fields. State is kept
// per sourceID because a single parser instance is shared across every
// file/stream processed within a mapreduce session; without this, the
// header row of every file after the first one would silently be mapped
// as a data row, corrupting aggregates.
type csvParser struct {
	defaultParser
	mu      sync.RWMutex
	headers map[string][]string
}

var _ Parser = (*csvParser)(nil)

func newCSVParser(hostname, timeZoneName string, timeZoneOffset int) (*csvParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &csvParser{}, err
	}
	return &csvParser{
		defaultParser: *defaultParser,
		headers:       make(map[string][]string),
	}, nil
}

func (p *csvParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
	header, installed := p.ensureHeader(sourceID, maprLine)
	if installed {
		return nil, ErrIgnoreFields
	}

	fields := make(map[string]string, p.fieldsCapacity)
	p.addDefaultFields(fields, maprLine)
	start := 0
	column := 0
	delimiter := protocol.CSVDelimiter[0]

	for {
		value, next, done := scanDelimitedField(maprLine, start, delimiter)
		if column >= len(header) {
			return fields, fmt.Errorf("CSV file seems corrupted, more fields than header values?")
		}
		p.addDynamicField(fields, header[column], value)
		column++
		if done {
			break
		}
		start = next
	}

	return fields, nil
}

// ensureHeader atomically checks for, and if necessary installs, the header
// for sourceID. It returns the effective header for the source and whether
// this call was the one that installed it. Only the goroutine that actually
// installs the header should tell its caller to ignore the current line
// (i.e. return ErrIgnoreFields); any racing goroutine on the same sourceID
// sees installed=false and proceeds to map its line against the installed
// header. The previous implementation split the check (RLock) from the
// install (Lock), so two goroutines could both observe "missing" and both
// report ErrIgnoreFields, silently dropping the loser's data row.
func (p *csvParser) ensureHeader(sourceID, maprLine string) ([]string, bool) {
	p.mu.RLock()
	if header, ok := p.headers[sourceID]; ok {
		p.mu.RUnlock()
		return header, false
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if header, ok := p.headers[sourceID]; ok {
		return header, false
	}
	header := parseHeaderLine(maprLine)
	p.headers[sourceID] = header
	return header, true
}

func parseHeaderLine(maprLine string) []string {
	var header []string
	start := 0
	delimiter := protocol.CSVDelimiter[0]
	for {
		field, next, done := scanDelimitedField(maprLine, start, delimiter)
		header = append(header, field)
		if done {
			break
		}
		start = next
	}
	return header
}
