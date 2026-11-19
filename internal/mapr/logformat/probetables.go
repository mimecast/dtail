package logformat

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// ProbeTabler is implemented by parsers whose lines belong to tables (the
// MAPREDUCE:<TABLE> of the default format) and that can tell a line's table
// without parsing its fields. It is optional and serves the probe twice:
// a ProbeTableScanner lists the tables of every line a probe scanned, also
// of the lines the sample's limits did not keep, so that the table list is
// as complete as the scanned window allows; and the line filter of a
// table-targeted probe (ProbeTableScanner.Matcher) keeps exactly the lines
// the format assigns to the table. The answer may be approximate (a line it
// names a table for may still fail to parse); the table is then listed
// without fields. A panic is recovered: the format's tabler is not used for
// the rest of the probe and the probe reports the format as failed
// (ProbeTableScanner.Failed). As with Prober, embedding a parser that
// implements it promotes its ProbeLineTable onto the new type.
type ProbeTabler interface {
	ProbeLineTable(line string) (table string, ok bool)
}

// ProbeTableScanner holds the ProbeTablers of the log formats of a probe.
// It collects the table names of scanned lines (Scan), at most
// ProbeMaxTables per format (names cut to ProbeMaxNameBytes), in the order
// first seen, and builds the line filter of a table-targeted probe
// (Matcher). It is not safe for concurrent use.
type ProbeTableScanner struct {
	formats []probeTableFormat
}

// probeTableFormat is the table scan state of one log format.
type probeTableFormat struct {
	name   string
	tabler ProbeTabler
	tables []string
	// failure is the recovered panic of the tabler, which is not called
	// again once it is set.
	failure error
}

// NewProbeTableScanner returns a scanner for the formats of names whose
// parser (built with a nil query and hostname, as Probe builds it)
// implements ProbeTabler; the others are left out.
func NewProbeTableScanner(names []string, hostname string) *ProbeTableScanner {
	scanner := &ProbeTableScanner{}
	for _, name := range names {
		parser, err := NewParserWithHostname(name, nil, hostname)
		if err != nil {
			continue
		}
		if tabler, ok := parser.(ProbeTabler); ok {
			scanner.formats = append(scanner.formats, probeTableFormat{name: name, tabler: tabler})
		}
	}
	return scanner
}

// Scan records the table of line for every format of the scanner that
// names one. A nil scanner does nothing.
func (s *ProbeTableScanner) Scan(line []byte) {
	if s == nil || len(s.formats) == 0 {
		return
	}
	text := string(line)
	for i := range s.formats {
		format := &s.formats[i]
		table, ok := format.lineTable(text)
		if !ok {
			continue
		}
		table = truncateUTF8(table, ProbeMaxNameBytes)
		if len(format.tables) < ProbeMaxTables && !slices.Contains(format.tables, table) {
			format.tables = append(format.tables, strings.Clone(table))
		}
	}
}

// Matcher returns the line filter of a table-targeted probe of table. A
// line one of the scanner's formats assigns to a table (ProbeTabler) is kept
// if that table is table; a line no ProbeTabler claims is kept if it holds
// table as a word of its own (ProbeTableLineMatcher), for the formats that
// name tables without implementing ProbeTabler (a custom Prober). Only lines
// holding the name are looked at closer, so the filter is cheap for the
// others. The matcher of the empty table keeps every line; a nil scanner's
// matcher is the word match alone.
func (s *ProbeTableScanner) Matcher(table string) func(line []byte) bool {
	word := ProbeTableLineMatcher(table)
	if s == nil || len(s.formats) == 0 || table == "" {
		return word
	}
	name := []byte(table)
	return func(line []byte) bool {
		if !bytes.Contains(line, name) {
			return false
		}
		text := string(line)
		claimed := false
		for i := range s.formats {
			lineTable, ok := s.formats[i].lineTable(text)
			if ok && lineTable == table {
				return true
			}
			claimed = claimed || ok
		}
		return !claimed && word(line)
	}
}

// Failed returns the formats whose ProbeTabler panicked, with the
// recovered panic, by format name.
func (s *ProbeTableScanner) Failed() map[string]error {
	if s == nil {
		return nil
	}
	failed := make(map[string]error)
	for _, format := range s.formats {
		if format.failure != nil {
			failed[format.name] = format.failure
		}
	}
	return failed
}

// Tables returns the tables scanned for the format name, sorted; nil when
// the format names no tables or none was scanned.
func (s *ProbeTableScanner) Tables(name string) []string {
	if s == nil {
		return nil
	}
	for _, format := range s.formats {
		if format.name == name && len(format.tables) > 0 {
			return slices.Sorted(slices.Values(format.tables))
		}
	}
	return nil
}

// AddTables returns result with the tables scanned for the format name that
// it does not list yet added without fields, within ProbeMaxTables (its own
// tables first). A result built from a bounded sample of a window thus
// still lists the tables of the window's lines the sample did not keep. A
// result with per-table data (ProbeResult.TableVariables) gets an empty
// entry for each added table, as no kept line of it filled a variable.
func (s *ProbeTableScanner) AddTables(name string, result ProbeResult) ProbeResult {
	tables := s.Tables(name)
	if len(tables) == 0 {
		return result
	}
	collector := probeCollector{result: result}
	perTable := result.TableVariables != nil
	for _, table := range tables {
		collector.ensureTable(table)
		if perTable {
			collector.ensureTableVariables(table)
		}
	}
	return collector.result
}

// lineTable asks the format's tabler for the table of line, unless it
// panicked before; a panic is recovered and recorded in failure.
func (f *probeTableFormat) lineTable(line string) (table string, ok bool) {
	if f.failure != nil {
		return "", false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			f.failure = fmt.Errorf("panic in ProbeLineTable: %v", recovered)
			table, ok = "", false
		}
	}()
	return f.tabler.ProbeLineTable(line)
}

// ProbeTableLineMatcher returns the format-blind line filter of a
// table-targeted probe: it keeps a line holding table as a word of its own,
// i.e. not next to an ASCII letter, digit or '_' (so "STATS" keeps
// "MAPREDUCE:STATS|..." but not "MAPREDUCE:STATSX|..."). It may keep lines
// of other tables that mention the name; the probe result is filtered by
// table afterwards (ProbeResult.FilterTable). ProbeTableScanner.Matcher
// uses it for the lines no ProbeTabler claims. The matcher of the empty
// table keeps every line.
func ProbeTableLineMatcher(table string) func(line []byte) bool {
	word := []byte(table)
	return func(line []byte) bool {
		if len(word) == 0 {
			return true
		}
		for offset := 0; offset+len(word) <= len(line); {
			i := bytes.Index(line[offset:], word)
			if i < 0 {
				return false
			}
			start := offset + i
			end := start + len(word)
			if (start == 0 || !isTableWordByte(line[start-1])) &&
				(end == len(line) || !isTableWordByte(line[end])) {
				return true
			}
			offset = start + 1
		}
		return false
	}
}

// isTableWordByte reports whether c continues a table name word.
func isTableWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
