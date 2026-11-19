package logformat

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Caps of a probe result. They bound what a probe of a large or hostile sample
// can return: Probe applies them to every result, also to one a format's own
// Prober built.
const (
	// ProbeMaxTables is the number of tables kept; lines of later new tables
	// are dropped.
	ProbeMaxTables = 64
	// ProbeMaxFieldsPerTable is the number of fields kept per table (and of
	// variables kept in ProbeResult.Variables); later new fields are dropped.
	ProbeMaxFieldsPerTable = 256
	// ProbeMaxValuesPerField is the number of distinct sample values kept per
	// field or variable, in the order of first appearance.
	ProbeMaxValuesPerField = 5
	// ProbeMaxValueBytes is the length a sample value is cut to, at a UTF-8
	// character boundary. Values are compared after the cut.
	ProbeMaxValueBytes = 64
	// ProbeMaxNameBytes is the length table, field and variable names are
	// cut to, at a UTF-8 character boundary, so a custom probe can not return
	// e.g. a whole log line as a field name. (The csv probe takes no header
	// with a longer column at all, csvProbeHeader.)
	ProbeMaxNameBytes = 64
	// ProbeMaxNoteBytes is the length ProbeResult.Note is cut to.
	ProbeMaxNoteBytes = 256
)

// ProbeAnyTable is the table name a probe uses for the fields of formats that
// have no tables (all built-ins but the default format). A query of such a
// format selects its lines without a MAPREDUCE:<TABLE> filter. It equals the
// name of the empty table (MAPREDUCE: with no name) of the default format.
const ProbeAnyTable = ""

// errNilProbeParser is returned by ProbeWithParser for a nil parser.
var errNilProbeParser = errors.New("probe: nil parser")

// ProbeLine is one sampled log line and the source (file or stream) it was
// read from. The SourceID plays the role it plays for Parser.MakeFields.
type ProbeLine struct {
	Line     string
	SourceID string
}

// ProbeSample is the input of a probe: lines of one or more sources, the
// lines of each source in the order of the source. A stateful format reads
// the sample as its parser reads a file, e.g. csv takes the first line of
// every source as its header row.
type ProbeSample struct {
	Lines []ProbeLine
}

// ProbeResult describes the fields a log format found in a sample, for
// completion and display (the interactive `:fields`). It is plain data and
// encodes to JSON as {"tables": {...}, "tableVariables": {...},
// "variables": {...}, "note": "..."}.
type ProbeResult struct {
	// Tables maps a table name (MAPREDUCE:<TABLE> of the default format, or
	// ProbeAnyTable) to its bareword fields, each with up to
	// ProbeMaxValuesPerField distinct sample values (possibly none, e.g. for
	// a csv header column without data rows).
	Tables map[string]map[string][]string `json:"tables,omitempty"`
	// TableVariables maps a table of Tables to the $-variables the lines of
	// that table filled, with their sample values, so that a query `from
	// TABLE` can be offered only what lines of TABLE carry: never the
	// format's KnownVariables the lines did not show. The generic probe
	// (and so the default and csv probes) sets it for every table of its
	// result, an empty map for a table none of whose lines it parsed (e.g.
	// one only a ProbeTableScanner found). It is per-table data of the
	// format once it is not nil: Probe then gives every table of Tables an
	// entry (empty if the Prober set none) and drops the entries of tables
	// Tables does not list. A Prober may leave it nil (a result built
	// before it existed stays valid): the format then has no per-table data
	// and VariablesOf falls back to Variables.
	TableVariables map[string]map[string][]string `json:"tableVariables,omitempty"`
	// Variables maps the $-variables of the format to their sample values.
	// Probe adds the format's KnownVariables that the sample did not show,
	// without values.
	Variables map[string][]string `json:"variables,omitempty"`
	// Note is an optional remark of a custom probe, shown with the fields.
	Note string `json:"note,omitempty"`
}

// Prober is implemented by parsers whose log format probes a sample in a way
// of its own, e.g. to name tables, to list a fixed schema without sampling, to
// hide fields or to supply better sample values. It is optional, like
// FieldsIntoParser and SourceReleaser: Probe falls back to ProbeWithParser for
// parsers that do not implement it, so every registered format can be probed.
// A Prober is called on a parser built for the probe alone (with a nil query,
// so it fills all fields) and may call ProbeWithParser and adjust its result.
// This, and the release of the sample's sources after a probe, requires that
// a ParserFactory returns a new parser on every call, as the built-in ones do:
// a factory sharing one parser between callers would have a probe release
// source ids of a parser in use elsewhere.
//
// HAZARD for parser authors: as with FieldsIntoParser, embedding another
// parser promotes its Probe, e.g. the default format's grouping by
// MAPREDUCE:<TABLE>, onto the new type; hold a reused parser in a named field.
type Prober interface {
	Probe(sample ProbeSample) (ProbeResult, error)
}

// VariablesOf returns the $-variables the lines of table filled, with their
// sample values: TableVariables[table], empty (not nil) for a table the
// per-table data does not list. Only a result without per-table data
// (TableVariables nil, a Prober that does not set it) falls back to the
// format-wide Variables, KnownVariables without values included. The
// result shares the maps of r.
func (r ProbeResult) VariablesOf(table string) map[string][]string {
	if r.TableVariables == nil {
		return r.Variables
	}
	if variables, ok := r.TableVariables[table]; ok {
		return variables
	}
	return map[string][]string{}
}

// FilterTable returns the part of r about table (compared exactly, as a
// query's from clause names it in upper case): Tables with that table only
// and TableVariables with its variables (VariablesOf, so the format-wide
// ones only for a result without per-table data), but no format-wide
// Variables, which are about all lines. The note is kept. It reports false
// when r does not list table. A table-targeted probe answers with it.
func (r ProbeResult) FilterTable(table string) (ProbeResult, bool) {
	fields, ok := r.Tables[table]
	if !ok {
		return ProbeResult{}, false
	}
	return ProbeResult{
		Tables:         map[string]map[string][]string{table: fields},
		TableVariables: map[string]map[string][]string{table: r.VariablesOf(table)},
		Note:           r.Note,
	}, true
}

// Empty reports whether the result holds no table, variable or note.
func (r ProbeResult) Empty() bool {
	return len(r.Tables) == 0 && len(r.Variables) == 0 && r.Note == ""
}

// Probe probes sample with the log format logFormatName: it builds the
// format's parser (with a nil query and hostname as $hostname) and uses its
// Prober when it implements one, otherwise ProbeWithParser. Every result is cut
// to the Probe* caps, and a non-empty one gets the format's KnownVariables
// (format-wide only: TableVariables keep what lines of a table filled, see
// ProbeResult.TableVariables).
//
// A format whose parser cannot be built (unknown names, the custom1/custom2
// templates, the mimecast stubs of the open source build) or, with the generic
// probe, that parses no line of the sample yields an empty result and no
// error, so a caller can probe every name of Names() and leave out the empty
// results. A format's own Prober may report fields anyway: csv lists the
// columns of the header rows also when no data line parses. An error is
// returned only when a Prober failed or the parser panicked.
//
// The parser is built from the format's ParserFactory, which must return a
// new parser on every call (see Prober).
func Probe(logFormatName, hostname string, sample ProbeSample) (ProbeResult, error) {
	parser, err := NewParserWithHostname(logFormatName, nil, hostname)
	if err != nil {
		return ProbeResult{}, nil
	}
	result, err := runProbe(parser, sample)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("probe %q log format: %w", logFormatName, err)
	}
	result = capProbeResult(result)
	if !result.Empty() {
		addKnownVariables(&result, logFormatName)
	}
	return result, nil
}

// ProbeWithParser is the generic probe: it parses every line of sample with
// parser, skips lines the parser ignores (ErrIgnoreFields) or fails on, and
// collects the fields of the others under ProbeAnyTable (bareword fields) and
// in Variables and TableVariables[ProbeAnyTable] ($-variables), with up to
// ProbeMaxValuesPerField distinct values each. The parser should be built with a nil query
// (mapr.ParserFieldPlan.AllFields), as Probe does, or it fills only the
// fields of its query. A parser implementing SourceReleaser is released of
// every source of the sample afterwards.
func ProbeWithParser(parser Parser, sample ProbeSample) (ProbeResult, error) {
	return probeLines(parser, sample, func(string) string { return ProbeAnyTable })
}

// runProbe calls the Prober of parser or the generic probe and turns a panic
// of the parser into an error: a probe runs every registered format over lines
// that format may never see in a query.
func runProbe(parser Parser, sample ProbeSample) (result ProbeResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = ProbeResult{}, fmt.Errorf("panic: %v", recovered)
		}
	}()
	if prober, ok := parser.(Prober); ok {
		return prober.Probe(sample)
	}
	return ProbeWithParser(parser, sample)
}

// probeLines parses the lines of sample with parser and collects the fields
// of every parsed line under the table tableOf returns for it.
func probeLines(parser Parser, sample ProbeSample, tableOf func(line string) string) (ProbeResult, error) {
	if isNilParser(parser) {
		return ProbeResult{}, errNilProbeParser
	}
	var collector probeCollector
	fields := make(map[string]string)
	for _, line := range sample.Lines {
		parsed, err := MakeFieldsInto(parser, fields, line.Line, line.SourceID)
		if err != nil {
			continue
		}
		table := tableOf(line.Line)
		collector.addFields(table, parsed)
		collector.ensureTableVariables(table)
	}
	releaseProbeSources(parser, sample)
	return collector.result, nil
}

// releaseProbeSources releases every source of sample from a parser keeping
// state per source, as the aggregator does once a source is done.
func releaseProbeSources(parser Parser, sample ProbeSample) {
	releaser, ok := parser.(SourceReleaser)
	if !ok {
		return
	}
	released := make(map[string]struct{})
	for _, line := range sample.Lines {
		if _, done := released[line.SourceID]; done {
			continue
		}
		released[line.SourceID] = struct{}{}
		releaser.ReleaseSource(line.SourceID)
	}
}

// addKnownVariables adds the KnownVariables of the format that the result
// does not list yet, without sample values.
func addKnownVariables(result *ProbeResult, logFormatName string) {
	known, enumerable := KnownVariables(logFormatName)
	if !enumerable {
		return
	}
	collector := probeCollector{result: *result}
	for _, name := range known {
		collector.ensureVariable(name)
	}
	*result = collector.result
}

// capProbeResult returns result cut to the Probe* caps. It copies the result,
// so the caps also hold for a Prober that built a larger one, and goes through
// tables, fields and variables in sorted order, so what is dropped does not
// depend on map iteration order. The TableVariables of a table the capped
// Tables do not list are dropped, and a result with per-table data
// (TableVariables not nil) gets an entry, empty if need be, for every table
// it keeps; a table's variables are capped like its fields
// (ProbeMaxFieldsPerTable).
func capProbeResult(result ProbeResult) ProbeResult {
	var collector probeCollector
	for _, table := range sortedKeys(result.Tables) {
		fields := result.Tables[table]
		collector.ensureTable(table)
		for _, field := range sortedKeys(fields) {
			collector.addValues(table, field, fields[field])
		}
	}
	if result.TableVariables != nil {
		for table := range collector.result.Tables {
			collector.ensureTableVariables(table)
		}
	}
	for _, table := range sortedKeys(result.TableVariables) {
		variables := result.TableVariables[table]
		for _, name := range sortedKeys(variables) {
			collector.addTableVariableValues(table, name, variables[name])
		}
	}
	for _, name := range sortedKeys(result.Variables) {
		collector.addVariableValues(name, result.Variables[name])
	}
	collector.result.Note = strings.Clone(truncateUTF8(result.Note, ProbeMaxNoteBytes))
	return collector.result
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// truncateUTF8 cuts value to at most maxBytes bytes without splitting a UTF-8
// character. The result shares the memory of value.
func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
