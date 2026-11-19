package logformat

// csvProbeMinColumns is the number of columns a csv probe needs: in a header
// row (counting only the columns csvProbeColumn takes) and in a data row. One
// column means no separator at all, which any line of another format has.
const csvProbeMinColumns = 2

// csvProbeSource is what the csv probe knows of one source of a sample: the
// columns of its header row (the first line) the probe lists, nil when that
// is no plausible csv header, the header's full width (all its columns), and
// how many of its data rows were sampled and how many of them parse as the
// parser reads them.
type csvProbeSource struct {
	columns  []string
	width    int
	rows     int
	matching int
}

// plausible tells whether the source looks like a csv file: a plausible
// header and, if data rows were sampled, at least half of them of
// csvProbeMinColumns up to the header's width (the parser takes rows with
// fewer columns than the header and rejects only wider ones). A file of
// another format whose first line happens to hold a comma (e.g. a message of
// dserver's `|`-separated log) fails here, as its other lines mostly hold
// none.
func (s *csvProbeSource) plausible() bool {
	return s.columns != nil && 2*s.matching >= s.rows
}

// add counts the data row line of the source.
func (s *csvProbeSource) add(line string) {
	s.rows++
	record, err := parseCSVRecord(line)
	if err == nil && len(record) >= csvProbeMinColumns && len(record) <= s.width {
		s.matching++
	}
}

// Probe is the csv format's own probe: the generic probe, plus the columns of
// the header row (the first line) of every source of the sample, so a column
// is listed even when no data row was sampled. The fields keep ProbeAnyTable,
// as a csv query selects every line.
//
// Only sources that look like csv files (csvProbeSource.plausible) are
// probed. Every other log format's file would otherwise turn up as csv too,
// its first line taken as a header of one column named after that whole line
// and its other lines as values of it, e.g. in `:fields` and the Tab
// completion of a dserver log. A sample with no csv source yields an empty
// result, so the csv format is left out. This is a heuristic with two
// limits: a csv file of a single column is never probed (dmap still queries
// it), and a free-text log most of whose lines hold one comma (or a few, no
// more than its first line) is probed as csv all the same.
func (p *csvParser) Probe(sample ProbeSample) (ProbeResult, error) {
	sample, headers := csvProbeSources(sample)
	if len(sample.Lines) == 0 {
		return ProbeResult{}, nil
	}
	result, err := ProbeWithParser(p, sample)
	if err != nil {
		return result, err
	}
	dropUnlistedCSVFields(result.Tables[ProbeAnyTable], headers)
	collector := probeCollector{result: result}
	for _, header := range headers {
		for _, column := range header {
			collector.ensureField(ProbeAnyTable, column)
		}
	}
	// Per-table data also when only header rows were sampled: no data line
	// filled a variable then (not the format's KnownVariables).
	collector.ensureTableVariables(ProbeAnyTable)
	return collector.result, nil
}

// csvProbeSources returns the lines of the sample's plausible csv sources, in
// their order, and the listed header columns of those sources in the order
// the sources first appear.
func csvProbeSources(sample ProbeSample) (ProbeSample, [][]string) {
	sources := make(map[string]*csvProbeSource)
	var order []string
	for _, line := range sample.Lines {
		source, ok := sources[line.SourceID]
		if !ok {
			sources[line.SourceID] = newCSVProbeSource(line.Line)
			order = append(order, line.SourceID)
			continue
		}
		if source.columns != nil {
			source.add(line.Line)
		}
	}

	var kept ProbeSample
	for _, line := range sample.Lines {
		if sources[line.SourceID].plausible() {
			kept.Lines = append(kept.Lines, line)
		}
	}
	var headers [][]string
	for _, sourceID := range order {
		if source := sources[sourceID]; source.plausible() {
			headers = append(headers, source.columns)
		}
	}
	return kept, headers
}

// newCSVProbeSource reads the header row line of a source: it must parse as
// one csv record and keep at least csvProbeMinColumns columns csvProbeColumn
// takes, else the source has no columns (not plausible). The columns
// csvProbeColumn drops (e.g. after a trailing comma) still count in the
// header's width, as they do for the parser.
func newCSVProbeSource(line string) *csvProbeSource {
	header, err := parseCSVRecord(line)
	if err != nil {
		return &csvProbeSource{}
	}
	var columns []string
	for _, column := range header {
		if csvProbeColumn(column) {
			columns = append(columns, column)
		}
	}
	if len(columns) < csvProbeMinColumns {
		return &csvProbeSource{}
	}
	return &csvProbeSource{columns: columns, width: len(header)}
}

// dropUnlistedCSVFields deletes the fields of the generic probe that no
// listed header column names: the data rows fill the columns csvProbeColumn
// drops too, an over-long one under its name already cut to
// ProbeMaxNameBytes. Every other field is a listed column, as the parser
// names a row's values by its source's header and rejects wider rows.
func dropUnlistedCSVFields(fields map[string][]string, headers [][]string) {
	listed := make(map[string]struct{})
	for _, header := range headers {
		for _, column := range header {
			listed[column] = struct{}{}
		}
	}
	for field := range fields {
		if _, ok := listed[field]; !ok {
			delete(fields, field)
		}
	}
}

// csvProbeColumn tells whether the probe lists a header column: not an empty
// one, which no query can name, and none longer than ProbeMaxNameBytes, which
// the probe caps would cut to a name the parser never fills.
func csvProbeColumn(column string) bool {
	return column != "" && len(column) <= ProbeMaxNameBytes
}
