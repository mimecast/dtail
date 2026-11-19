package logformat

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

// csvParser parses CSV log lines. The first line encountered for a given
// sourceID is treated as the column header and stored so that subsequent
// lines from the same source can be mapped to named fields. State is kept
// per sourceID because a single parser instance is shared across every
// file/stream processed within a mapreduce session; without this, the
// header row of every file after the first one would silently be mapped
// as a data row, corrupting aggregates.
//
// The parser cannot tell a header row from a data row, so it relies on the
// ordering promised by Parser.MakeFields: a source's first line is parsed
// before any other line of that source. Should a data row come first
// nevertheless, it becomes the header, and the real header row is later
// mapped as data. The aggregator therefore gives every file read, and every
// rewrite of a file after an in-place truncation the follow reader detects, a
// sourceID of its own, parsed in file order, and releases it via
// ReleaseSource. A truncation the reader misses (see fs.ReadFile.truncated)
// keeps the old sourceID and header.
//
// The defaultParser is held in a named field rather than embedded: embedding
// would promote defaultParser.MakeFieldsInto onto csvParser, so a parser that
// overrode only MakeFields would keep compiling while the allocation-free path
// parsed its lines in the wrong layout (see the FieldsIntoParser doc comment).
type csvParser struct {
	base    defaultParser
	mu      sync.RWMutex
	headers map[string][]string
}

var _ Parser = (*csvParser)(nil)
var _ FieldsIntoParser = (*csvParser)(nil)
var _ queryAwareParser = (*csvParser)(nil)
var _ SourceReleaser = (*csvParser)(nil)
var _ WhitespacePreservingParser = (*csvParser)(nil)

func newCSVParser(hostname, timeZoneName string, timeZoneOffset int) (*csvParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &csvParser{}, err
	}
	return &csvParser{
		base:    *defaultParser,
		headers: make(map[string][]string),
	}, nil
}

func (p *csvParser) setQuery(query *mapr.Query) {
	p.base.setQuery(query)
}

// PreserveWhitespace keeps leading and trailing spaces in CSV fields.
func (p *csvParser) PreserveWhitespace() bool { return true }

func (p *csvParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
	fields := make(map[string]string, p.base.fieldsCapacity)
	if err := p.MakeFieldsInto(fields, maprLine, sourceID); err != nil {
		if errors.Is(err, ErrIgnoreFields) {
			return nil, err
		}
		return fields, err
	}
	return fields, nil
}

// ReleaseSource forgets the header of sourceID once the source is done.
func (p *csvParser) ReleaseSource(sourceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.headers, sourceID)
}

// MakeFieldsInto parses maprLine as a CSV data row into the caller's map.
func (p *csvParser) MakeFieldsInto(dst map[string]string, maprLine, sourceID string) error {
	clear(dst)
	if strings.ContainsAny(maprLine, "\r\n") {
		return errors.New("multiline CSV fields are unsupported")
	}
	header, installed, err := p.ensureHeader(sourceID, maprLine)
	if err != nil {
		return err
	}
	if installed {
		return ErrIgnoreFields
	}

	p.base.addDefaultFields(dst, maprLine)
	if strings.ContainsRune(maprLine, '"') {
		values, err := parseCSVRecord(maprLine)
		if err != nil {
			return err
		}
		if len(values) > len(header) {
			return fmt.Errorf("CSV file seems corrupted, more fields than header values?")
		}
		for column, value := range values {
			p.base.addDynamicField(dst, header[column], value)
		}
		return nil
	}
	// Keep the common, unquoted path allocation-free.
	start := 0
	column := 0
	delimiter := protocol.CSVDelimiter[0]

	for {
		value, next, done := scanDelimitedField(maprLine, start, delimiter)
		if column >= len(header) {
			return fmt.Errorf("CSV file seems corrupted, more fields than header values?")
		}
		p.base.addDynamicField(dst, header[column], value)
		column++
		if done {
			break
		}
		start = next
	}

	return nil
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
func (p *csvParser) ensureHeader(sourceID, maprLine string) ([]string, bool, error) {
	p.mu.RLock()
	if header, ok := p.headers[sourceID]; ok {
		p.mu.RUnlock()
		return header, false, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if header, ok := p.headers[sourceID]; ok {
		return header, false, nil
	}
	header, err := parseCSVRecord(maprLine)
	if err != nil {
		return nil, false, fmt.Errorf("CSV header: %w", err)
	}
	p.headers[sourceID] = header
	return header, true, nil
}

// parseCSVRecord returns owned strings: headers outlive the borrowed input
// buffer, and encoding/csv copies the decoded record into its own storage.
func parseCSVRecord(maprLine string) ([]string, error) {
	if maprLine == "" {
		return []string{""}, nil
	}
	r := csv.NewReader(strings.NewReader(maprLine))
	r.Comma = rune(protocol.CSVDelimiter[0])
	r.FieldsPerRecord = -1
	fields, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("invalid CSV record (multiline fields are unsupported): %w", err)
	}
	return fields, nil
}
