package clients

import (
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/funcs"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

// queryCatalogHint is listed with the candidates of a query word that would
// be completed from the field catalog while that holds nothing yet (a
// session without files gets the no-files hint instead, catalogEmptyHint).
const queryCatalogHint = "(no field data from the servers yet: only the query language completes)"

// queryCompletion is what completing a word of a --query value found: the
// candidates for the word (in the query's own syntax, not yet escaped for
// the shell quote) and an optional hint line.
type queryCompletion struct {
	word string
	// groups are the candidates by what they are (clauses that may come
	// next, aggregations, fields, $variables, operators, values, ...), in
	// the order they are offered; candidates are made of them.
	groups     []candidateGroup
	candidates []string
	// heading says what is expected at the word (queryHeading), shown
	// above the groups when they are listed.
	heading string
	hint    string
	// table is the query's from table (upper case), typed before the word
	// or after the cursor, empty if none, for the table probe the editor
	// asks for (completion.probeTable).
	table string
	// afterItem tells that the word under the cursor was a finished item
	// ending in ")" (count(x), md5sum($y)) and the candidates are those of
	// the position after the blank that follows it (completeAfterItem):
	// they are listed, never inserted, not even a single one, as a comma
	// may follow instead.
	afterItem bool
	// lead is what Tab inserts in place of the word before the position
	// the candidates are of, which it lists in the same Tab, never
	// inserting one: the blank after a finished item when the item ends
	// the query (afterItem; with text after the cursor it inserts nothing,
	// so it neither doubles a blank nor splits a token), and "from " that
	// starts a query while the catalog knows several tables
	// (completeFromFirst).
	lead string
}

// insertion returns what Tab inserts in place of the word, "" for
// nothing, and the candidates to list, nil when the insertion completes
// the word (decodedReplacement). After a finished item (afterItem) or
// with a lead it inserts at most the lead and lists every candidate, both
// in one Tab.
func (q queryCompletion) insertion() (string, []string) {
	if q.afterItem || q.lead != "" {
		return q.lead, q.candidates
	}
	return decodedReplacement(q.word, q.candidates)
}

// itemBlankSize returns the length of inserted, the replacement as put on
// the line, when it is the blank after a finished item (afterItem with
// lead), else 0 (completion.itemBlank).
func (q queryCompletion) itemBlankSize(inserted string) int {
	if !q.afterItem || q.lead == "" {
		return 0
	}
	return len(inserted)
}

// listInto sets the listing of q in result: its candidates (listed, from
// insertion), heading, groups and hint, and its table for the table probe.
func (q queryCompletion) listInto(result completion, listed []string) completion {
	result.candidates = listed
	if listed != nil {
		result.heading, result.groups = q.heading, q.groups
	}
	result.hint, result.probeTable = q.hint, q.table
	return result
}

// candidateGroup is a group of query candidates of one kind, labelled with
// what they are (queryCandidateKinds), so that a listing shows them on a
// line of their own after the label, painted in the kind's style.
type candidateGroup struct {
	label      string
	kind       candidateKind
	candidates []string
}

// completeQueryWord completes the word at the end of query, a --query value
// typed up to the cursor, followed by tail (the rest of the value after the
// cursor), from the query language (mapr.AnalyzePartial and the vocabulary
// tables) and the field catalog. What it offers is guided (guideQuery,
// interactive_complete_query_guide.go): one step at a time, and fields,
// $variables and values only of the query's table once it names one, on
// either side of the cursor (queryOutline). Right after a finished item
// ending in ")" it lists what follows the blank after it, which Tab
// inserts too at the end of the query (completeAfterItem); where it
// inserts from alone, it lists the tables after it (completeFromFirst). It
// returns no candidates where nothing can be offered (a number, an outfile
// path).
func completeQueryWord(query, tail string, catalog catalogSnapshot) queryCompletion {
	partial := mapr.AnalyzePartial(query)
	result := completeQueryPartial(partial, tail, catalog)
	if next, ok := completeAfterItem(partial, result, tail, catalog); ok {
		return next
	}
	if next, ok := completeFromFirst(partial, result, tail, catalog); ok {
		return next
	}
	return result
}

// completeFromFirst completes a Tab that would insert from alone with an
// empty word (a query begun with Tab while the catalog knows several tables
// of its format, nextClauses) as that Tab and the next one: it inserts
// "from " (lead) and lists the tables that may follow it, under their
// heading, so that one Tab starts the query and shows its tables. It
// reports false elsewhere, also when no table may follow (none known: the
// Tab inserts from with tableNameHint).
func completeFromFirst(partial mapr.Partial, own queryCompletion, tail string,
	catalog catalogSnapshot) (queryCompletion, bool) {

	const from = "from "
	if partial.Word != "" || !slices.Equal(own.candidates, []string{from}) {
		return queryCompletion{}, false
	}
	next := completeQueryPartial(mapr.AnalyzePartial(partial.Query+from), tail, catalog)
	if len(next.candidates) == 0 {
		return queryCompletion{}, false
	}
	next.word, next.lead = partial.Word, from
	return next, true
}

// completeAfterItem completes Tab right after a finished item ending in
// ")" (select count(x), set $a = md5sum($b), order by count(x), a where
// value such as eq md5sum($x)) as Tab after the blank that would follow
// it: it lists the next step of the guided order with its hint (a comma
// adds another item), even a single next clause, and inserts the blank
// when the item ends the query (afterItem, lead), so that Tab right
// after the ")" it inserted continues the query. With text after the
// cursor (a blank, a comma, the rest of a word) nothing is inserted, also
// when nothing comes next, so that no blank is doubled (order by
// count(x)| limit 3) and no token split. The word must end in ")", be no
// quoted or escaped word and no token the parser rejects (count(x)) or an
// unclosed md5sum(maskdigits(x)), and its own completion may add nothing
// but that blank (an order by item completes to itself and a blank); the
// position after the blank must expect a clause. It reports false
// otherwise, and when nothing comes next at the end of the query, where
// Tab completes the word as it is.
func completeAfterItem(partial mapr.Partial, own queryCompletion, tail string,
	catalog catalogSnapshot) (queryCompletion, bool) {

	word := partial.Word
	if !strings.HasSuffix(word, ")") || partial.InQuote || partial.InBacktick ||
		slices.ContainsFunc(own.candidates, func(c string) bool { return c != word+" " }) {
		return queryCompletion{}, false
	}
	tokens := partial.Tokens
	if len(tokens) == 0 || tokens[len(tokens)-1].Kind == mapr.TokenUnknown {
		return queryCompletion{}, false
	}
	after := mapr.AnalyzePartial(partial.Query + " ")
	if !after.Expect.Has(mapr.ExpectClauseKeyword) {
		return queryCompletion{}, false
	}
	next := completeQueryPartial(after, tail, catalog)
	if len(next.candidates) == 0 && tail == "" {
		return queryCompletion{}, false
	}
	next.afterItem = true
	if tail == "" {
		next.lead = " "
	}
	return next, true
}

// completeQueryPartial completes the word of partial, the analysis of a
// query typed up to the cursor, followed by tail (completeQueryWord).
func completeQueryPartial(partial mapr.Partial, tail string, catalog catalogSnapshot) queryCompletion {
	outline := newQueryOutline(partial, tail)
	source := newQueryCandidateSource(partial, outline, catalog)
	guide := guideQuery(partial)
	result := queryCompletion{word: partial.Word, groups: source.groups(guide.expect), table: outline.table,
		heading: queryHeading(partial, guide.expect)}
	result.candidates = flattenGroups(result.groups)
	if partial.Clause == "" {
		// Before the first clause nothing is "next" to anything yet.
		result.groups = relabel(result.groups, "next", queryClauseLabel)
	}
	result.hint = joinLines(guide.hint, source.tableHint(result.groups), source.tableNameHint(guide.expect))
	if catalog.empty() && (guide.expect.Has(catalogExpectations) || source.atFirstClause(guide.expect)) {
		result.hint = joinLines(result.hint, catalogEmptyHint(catalog, queryCatalogHint))
	}
	return result
}

// atFirstClause reports whether the word is where the first clause of the
// query starts (a clause keyword expected, no word typed, neither from nor
// select in the query): with an empty catalog Tab inserts from there with
// tableNameHint, and queryCatalogHint explains why no table follows.
func (s queryCandidateSource) atFirstClause(expect mapr.Expect) bool {
	return expect.Has(mapr.ExpectClauseKeyword) && s.partial.Word == "" &&
		!s.outline.isUsed("from") && !s.outline.isUsed("select")
}

// catalogExpectations are the expected kinds whose candidates come from the
// field catalog.
const catalogExpectations = mapr.ExpectField | mapr.ExpectAggregationField |
	mapr.ExpectTable | mapr.ExpectValue

// queryCandidateKinds maps every expected kind of word to the function that
// lists its candidates, in the order candidates are offered (and, as it
// continues a condition, before the next clauses), to the label of their
// group and to the kind they are styled as in the listing. Fields and
// $variables get groups of their own (queryCandidateSource.groups).
var queryCandidateKinds = []struct {
	expect     mapr.Expect
	candidates func(queryCandidateSource) []string
	label      string
	kind       candidateKind
}{
	{mapr.ExpectAnd, func(queryCandidateSource) []string { return []string{"and "} }, "next", candidateKeyword},
	{mapr.ExpectClauseKeyword, queryKeywordCandidates, "next", candidateKeyword},
	{mapr.ExpectAggregation, queryAggregationCandidates, "aggregation", candidateFunction},
	{mapr.ExpectAggregationField, queryAggregationFieldCandidates, "field", candidateField},
	{mapr.ExpectField, queryFieldCandidates, "field", candidateField},
	{mapr.ExpectSetVariable, queryVariableCandidates, "variable", candidateVariable},
	{mapr.ExpectSetFunction, queryFunctionCandidates, "function", candidateFunction},
	{mapr.ExpectWhereOperator, queryOperatorCandidates, "operator", candidateOperator},
	{mapr.ExpectValue, queryValueCandidates, "value", candidateValue},
	{mapr.ExpectAssign, func(queryCandidateSource) []string { return []string{"= "} }, "operator", candidateOperator},
	{mapr.ExpectBy, func(s queryCandidateSource) []string { return s.outline.leadCandidates("by") },
		"next", candidateKeyword},
	{mapr.ExpectAppend, func(s queryCandidateSource) []string { return s.outline.leadCandidates("append") },
		"next", candidateKeyword},
	{mapr.ExpectTable, queryTableCandidates, "table", candidateTable},
	{mapr.ExpectLogFormat, queryLogFormatCandidates, "log format", candidateKeyword},
	{mapr.ExpectSelectItem, querySelectItemCandidates, "select item", candidateField},
}

// queryCandidateSource is what the candidate functions draw from: the
// analysis of the query up to the cursor, the outline of the whole query
// and the log formats whose catalog data applies.
type queryCandidateSource struct {
	partial mapr.Partial
	outline queryOutline
	// logFormats are the formats whose fields and $variables are offered:
	// the query's effective log format and, while neither a table nor a
	// log format is typed (the query is still incomplete; without a table
	// the generic format would apply), also the servers' default format,
	// which the query uses once it names a table.
	logFormats []string
	// tableFormat is the format whose tables are offered after from: the
	// typed log format, else the servers' default one.
	tableFormat string
	catalog     catalogSnapshot
}

func newQueryCandidateSource(partial mapr.Partial, outline queryOutline,
	catalog catalogSnapshot) queryCandidateSource {

	defaultFormat := catalog.DefaultLogFormat
	if defaultFormat == "" {
		defaultFormat = "default"
	}
	effective := mapr.Partial{Table: outline.table, LogFormat: outline.logFormat}.EffectiveLogFormat(defaultFormat)
	source := queryCandidateSource{partial: partial, outline: outline, catalog: catalog,
		tableFormat: defaultFormat, logFormats: []string{effective}}
	if outline.logFormat != "" {
		source.tableFormat = outline.logFormat
	}
	if outline.table == "" && outline.logFormat == "" && effective != defaultFormat {
		source.logFormats = append(source.logFormats, defaultFormat)
	}
	return source
}

// groups returns the candidates of the kinds in expect that match the word,
// grouped by queryCandidateKinds with fields and $variables apart, empty
// groups left out. A candidate is kept only in the first group offering
// it. Tables, fields, $variables and values of the catalog are log
// content: a candidate is printed in the listing and inserted into the
// edit line, so one holding a control or format character (an escape
// sequence, a bidirectional override) is dropped here, for every kind at
// once, as the path completion drops such names (readableFilesName).
func (s queryCandidateSource) groups(expect mapr.Expect) []candidateGroup {
	var groups []candidateGroup
	seen := map[string]bool{}
	for _, kind := range queryCandidateKinds {
		if !expect.Has(kind.expect) {
			continue
		}
		found := slices.DeleteFunc(kind.candidates(s), func(c string) bool { return seen[c] || hasHiddenRune(c) })
		found = matchingQueryWord(dedupe(found), s.partial.Word)
		for _, c := range found {
			seen[c] = true
		}
		if kind.kind != candidateField {
			groups = appendGroup(groups, candidateGroup{label: kind.label, kind: kind.kind, candidates: found})
			continue
		}
		isVariable := func(c string) bool { return strings.HasPrefix(c, "$") }
		variables := slices.DeleteFunc(slices.Clone(found), func(c string) bool { return !isVariable(c) })
		fields := slices.DeleteFunc(found, isVariable)
		groups = appendGroup(groups, candidateGroup{label: kind.label, kind: kind.kind, candidates: fields})
		groups = appendGroup(groups, candidateGroup{label: "variable", kind: candidateVariable, candidates: variables})
	}
	return groups
}

// appendGroup appends group unless it has no candidates.
func appendGroup(groups []candidateGroup, group candidateGroup) []candidateGroup {
	if len(group.candidates) == 0 {
		return groups
	}
	return append(groups, group)
}

// queryClauseLabel labels the clauses a query may start with.
const queryClauseLabel = "clause"

// relabel returns groups with every group labelled from labelled to instead.
func relabel(groups []candidateGroup, from, to string) []candidateGroup {
	for i := range groups {
		if groups[i].label == from {
			groups[i].label = to
		}
	}
	return groups
}

// flattenGroups returns the candidates of groups in order.
func flattenGroups(groups []candidateGroup) []string {
	var candidates []string
	for _, group := range groups {
		candidates = append(candidates, group.candidates...)
	}
	return candidates
}

// tableHint returns tableFirstHint where groups offer fields or $variables
// while the query names no table and the catalog knows tables of its
// format.
func (s queryCandidateSource) tableHint(groups []candidateGroup) string {
	if s.outline.table != "" || s.outline.isUsed("from") || !s.tablesKnown() {
		return ""
	}
	for _, group := range groups {
		if group.kind == candidateField || group.kind == candidateVariable {
			return tableFirstHint
		}
	}
	return ""
}

// tableNameHint returns tableNameHint where Tab starts the query with
// from (atFirstClause) or a table is expected after from while no server
// has answered yet: neither a table of the format is known nor did the
// servers answer without one (tableless, where the query starts at
// select). Not for a session that reads no files: no server can ever
// answer, and the no-files hint (catalogEmptyHint) says what to do.
func (s queryCandidateSource) tableNameHint(expect mapr.Expect) string {
	if s.catalog.NoFilesHint != "" {
		return ""
	}
	atTable := expect.Has(mapr.ExpectTable) && s.partial.Clause == "from"
	if (atTable || s.atFirstClause(expect)) && !s.tablesKnown() && !s.tableless() {
		return tableNameHint
	}
	return ""
}

// tablesKnown reports whether the catalog knows a table of the format
// tables are offered of.
func (s queryCandidateSource) tablesKnown() bool {
	for table := range s.catalog.Formats[s.tableFormat].Tables {
		if table != "" {
			return true
		}
	}
	return false
}

// tableless reports whether the servers answered (the catalog is not
// empty) without a table of the format tables are offered of: its lines
// only have fields of the empty table (logformat.ProbeAnyTable), as for
// csv and every built-in format but the default one, or the format found
// nothing at all, as the default format in plain logs, whose lines only
// generic parses. A from clause would select nothing there. Only an empty
// catalog means that no table is known yet (tableNameHint).
func (s queryCandidateSource) tableless() bool {
	return !s.catalog.empty() && !s.tablesKnown()
}

// closing returns what ends a word of the source: the closing double quote
// or backtick the word is in, otherwise sep (a blank or a parenthesis).
func (s queryCandidateSource) closing(sep string) string {
	if quote := s.quote(); quote != "" {
		return quote
	}
	return sep
}

// quote returns the double quote or backtick the word is in, if any.
func (s queryCandidateSource) quote() string {
	switch {
	case s.partial.InQuote:
		return `"`
	case s.partial.InBacktick:
		return "`"
	}
	return ""
}

// queryKeywordCandidates lists the clauses that may start at the word, as
// the guided order allows (queryOutline.nextClauses).
func queryKeywordCandidates(s queryCandidateSource) []string {
	tables := slices.DeleteFunc(queryTableCandidates(s), hasHiddenRune)
	return s.outline.nextClauses(s.partial.Word, tables, s.tableless())
}

func queryAggregationCandidates(queryCandidateSource) []string {
	return withSuffix(mapr.AggregationNames(), "(")
}

func queryFunctionCandidates(queryCandidateSource) []string {
	return withSuffix(funcs.Names(), "(")
}

func queryOperatorCandidates(queryCandidateSource) []string {
	var names []string
	for _, op := range mapr.WhereOperators() {
		names = append(names, op.Name)
	}
	return withSuffix(names, " ")
}

// queryLogFormatCandidates lists the log format names, closing the double
// quote the word may be in (logformat "csv" is the format csv).
func queryLogFormatCandidates(s queryCandidateSource) []string {
	return withSuffix(logformat.Names(), s.closing("")+" ")
}

// querySelectItemCandidates lists the select items for order by, written
// so that the parser reads each as that item (querySelectItemWords).
func querySelectItemCandidates(s queryCandidateSource) []string {
	return querySelectItemWords(s.partial.SelectItems, s.quote())
}

// queryFieldCandidates lists the fields of the query's table (of every
// table while none is typed) and the $variables of its log format. Inside a
// query string quote, AnalyzePartial expects a field only where a quoted
// token still is one (select, group by), not where it is a string literal
// (a where or set condition); there the fields are written for the quote
// and closed (quotedQueryWords).
func queryFieldCandidates(s queryCandidateSource) []string {
	names := s.offeredFields()
	if s.partial.InQuote {
		return withSuffix(quotedQueryWords(names, true), " ")
	}
	if s.inCall() {
		// A function argument, e.g. md5sum(: written plain and closed.
		return withSuffix(queryFieldWords(names, true), ")")
	}
	return withSuffix(queryFieldWords(names, s.partial.InBacktick), s.closing(" "))
}

// queryAggregationFieldCandidates lists the fields inside AGG(, closed by
// the parenthesis. They are written without backticks even when named like
// a keyword: the parser strips backticks only around a whole token, so
// count(`from`) would count a field named `from` with its backticks, while
// count(from) is no keyword token and counts the field from.
func queryAggregationFieldCandidates(s queryCandidateSource) []string {
	if s.partial.InQuote {
		return nil
	}
	return withSuffix(queryFieldWords(s.offeredFields(), true), s.closing(")"))
}

// offeredFields returns the fields and $variables that can be written at
// the word: in select none whose item (the name, or AGG(name) inside an
// aggregation, as selectCondition.FieldStorage) is selected already, which
// the parser rejects; after a where condition no field named and (in any
// case, backticks or not), which the parser reads as the joining and.
func (s queryCandidateSource) offeredFields() []string {
	names := append(s.fieldNames(), s.variableNames()...)
	p := s.partial
	return slices.DeleteFunc(names, func(name string) bool {
		item := name
		if p.InAggregation {
			item = p.Aggregation + "(" + name + ")"
		}
		return (p.Clause == "select" && slices.Contains(p.SelectItems, item)) ||
			(p.Clause == "where" && p.Expect.Has(mapr.ExpectAnd) && strings.EqualFold(name, "and"))
	})
}

// inCall reports whether the word is the argument of a call written right
// after its opening parenthesis (a set function such as md5sum(), where a
// field is written like inside AGG(.
func (s queryCandidateSource) inCall() bool {
	p := s.partial
	return !p.InBacktick && p.WordStart > 0 && p.Query[p.WordStart-1] == '('
}

func queryVariableCandidates(s queryCandidateSource) []string {
	return withSuffix(queryFieldWords(s.variableNames(), false), " ")
}

// queryTableCandidates lists the tables the servers found for the query's
// log format (and for every format when it has none).
func queryTableCandidates(s queryCandidateSource) []string {
	var tables []string
	for table := range s.catalog.Formats[s.tableFormat].Tables {
		if table != "" {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	if s.partial.InQuote {
		// from "STATS" is the table STATS: written as is and closed.
		return withSuffix(quotedQueryWords(tables, false), " ")
	}
	// Bare (a keyword in backticks) or, inside backticks, as is and closed.
	return withSuffix(queryFieldWords(tables, s.partial.InBacktick), s.closing(" "))
}

// queryValueCandidates lists the sample values of the where condition's
// field: double-quoted after a string operator (quotedValueWord; values
// containing a double quote, which the query language can not quote, or a
// line break are left out, as queryCandidateSource.groups leaves out every
// value with another control or format character), plain decimal numbers
// after a
// numeric one. A sample of the probe's
// full value length may have been cut (logformat.ProbeMaxValueBytes), so it
// is only offered for operators that compare a prefix or a part.
func queryValueCandidates(s queryCandidateSource) []string {
	op := s.partial.ValueOperator
	if op.Name == "" {
		return nil
	}
	var values []string
	for _, value := range s.samples(s.partial.ValueField) {
		if len(value) >= logformat.ProbeMaxValueBytes && !partialValueOperators[op.Name] {
			continue
		}
		switch {
		case op.Kind == mapr.NumericOperator:
			if decimalNumber.MatchString(value) && !s.partial.InQuote {
				values = append(values, value+" ")
			}
		case !strings.ContainsAny(value, "\n\r"):
			values = append(values, quotedValueWord(value, s.partial.InQuote)...)
		}
	}
	return values
}

// quotedValueWord writes a sample value as the double-quoted string of a
// where condition (only the rest and the closing quote when the word is in
// the quote already): the parser strips a pair of backticks around a quoted
// token too, so a value wrapped in backticks gets another pair
// (quotedQueryWord). A value with a double quote, which the query language
// can not quote, gives no word.
func quotedValueWord(value string, inQuote bool) []string {
	word, ok := quotedQueryWord(value, false)
	switch {
	case !ok:
		return nil
	case inQuote:
		return []string{word + " "}
	}
	return []string{`"` + word + " "}
}

// partialValueOperators compare a prefix or a part of a value, for which a
// sample cut at the probe's value length still works.
var partialValueOperators = map[string]bool{"contains": true, "ncontains": true,
	"lacks": true, "hasprefix": true, "nhasprefix": true}

// decimalNumber matches the plain decimal numbers offered after a numeric
// operator (no NaN, Inf or hexadecimal forms, which strconv would accept).
var decimalNumber = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// fieldNames returns the catalog fields of the query's table, or of every
// table of its log formats while the query names none.
func (s queryCandidateSource) fieldNames() []string {
	var names []string
	for _, name := range s.logFormats {
		for table, fields := range s.catalog.Formats[name].Tables {
			if s.outline.table != "" && table != s.outline.table {
				continue
			}
			for field := range fields {
				names = append(names, field)
			}
		}
	}
	sort.Strings(names)
	return dedupe(names)
}

// variableNames returns the $variables of the query's log formats. Once the
// query names a table, only those the lines of that table filled
// (tableVariables); otherwise the ones the servers saw and the ones the
// formats are known to fill.
func (s queryCandidateSource) variableNames() []string {
	var names []string
	for _, format := range s.logFormats {
		if variables, ok := s.tableVariables(format); ok {
			names = append(names, slices.Collect(maps.Keys(variables))...)
			continue
		}
		names = append(names, slices.Collect(maps.Keys(s.catalog.Formats[format].Variables))...)
		if known, ok := logformat.KnownVariables(format); ok {
			names = append(names, known...)
		}
	}
	sort.Strings(names)
	return dedupe(names)
}

// tableVariables returns the $variables, with sample values, that the lines
// of the query's table filled in format (catalogFormat.variablesOf), and
// whether the table scopes them: only when the query names a table and the
// catalog has per-table data of format. Without such data (no table of
// format in the catalog yet) the format-wide and known ones are offered.
func (s queryCandidateSource) tableVariables(format string) (map[string][]string, bool) {
	f := s.catalog.Formats[format]
	if s.outline.table == "" || f.TableVariables == nil {
		return nil, false
	}
	return f.variablesOf(s.outline.table), true
}

// samples returns the catalog's sample values of field (a field of the
// query's table or a $variable, of that table's lines once the query names
// one).
func (s queryCandidateSource) samples(field string) []string {
	var values []string
	for _, name := range s.logFormats {
		format := s.catalog.Formats[name]
		if variables, ok := s.tableVariables(name); ok {
			values = append(values, variables[field]...)
		} else {
			values = append(values, format.Variables[field]...)
		}
		// In table order, so the values come the same on every Tab.
		for _, table := range slices.Sorted(maps.Keys(format.Tables)) {
			if s.outline.table == "" || table == s.outline.table {
				values = append(values, format.Tables[table][field]...)
			}
		}
	}
	return dedupe(values)
}

// empty reports whether no server answered yet.
func (c catalogSnapshot) empty() bool {
	return len(c.Servers) == 0
}

// matchingQueryWord keeps the candidates that start with word, ignoring case
// (keywords and operators ignore case in queries; for the other words the
// candidate's own case is what gets inserted). A double-quoted value, or a
// field in backticks, also matches a word typed without its opening quote
// or backtick, which it then replaces.
func matchingQueryWord(candidates []string, word string) []string {
	lowerWord := strings.ToLower(word)
	var out []string
	for _, candidate := range candidates {
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, lowerWord) ||
			(!strings.HasPrefix(word, `"`) && strings.HasPrefix(strings.TrimPrefix(lower, `"`), lowerWord)) ||
			(!strings.HasPrefix(word, "`") && strings.HasPrefix(strings.TrimPrefix(lower, "`"), lowerWord)) {
			out = append(out, candidate)
		}
	}
	return out
}

func withSuffix(names []string, suffix string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name + suffix
	}
	return out
}

func dedupe(candidates []string) []string {
	seen := make(map[string]bool, len(candidates))
	out := candidates[:0:0]
	for _, candidate := range candidates {
		if !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	return slices.Clip(out)
}

// queryFlagNames are the spellings of the --query flag a value belongs to.
var queryFlagNames = []string{"--query", "-query"}

// completeQueryValue completes the word at the cursor when it is inside the
// value of :reload --query (after "--query " in an open quote or escaped, or
// in "--query=..."), in a mode whose :reload accepts --query. It reports
// false elsewhere, also where the value has not started (then the current
// query is inserted as before). The catalog is only read here. Candidates
// are matched and their common prefix taken on the decoded text, and only
// the replacement is escaped for the open shell quote of the value, so the
// line still splits into the intended query; a word whose bytes were not
// all read in that quote (it spans a quote change) is left alone.
func completeQueryValue(line string, pos int, args config.Args, catalog func() catalogSnapshot) (completion, bool) {
	before, after := line[:pos], line[pos:]
	rest, ok := reloadArguments(before)
	if !ok || !slices.Contains(interactiveReloadFlags(args), "--query ") {
		return completion{}, false
	}
	parsed := splitInteractiveArgsPartial(rest)
	value, valueStart, ok := queryValueOf(parsed)
	if !ok {
		return completion{}, false
	}
	query := completeQueryWord(value, queryValueTail(parsed, rest+after), catalog())
	unchanged := completion{line: line, pos: pos, probeTable: query.table}
	wordStart := valueStart + len(value) - len(query.word)
	if !parsed.suffixInQuote(wordStart) {
		return unchanged, true
	}
	rawStart := len(before) - len(rest) + parsed.rawOffset(wordStart, len(rest))
	replacement, listed := query.insertion()
	result := query.listInto(unchanged, listed)
	if replacement != "" {
		escaped := escapeForQuote(replacement, parsed.quote)
		result.line = before[:rawStart] + escaped + after
		result.pos = len(result.line) - len(after)
		result.changed = result.line != line
		result.itemBlank = query.itemBlankSize(escaped)
	}
	return result, true
}

// queryValueTail returns the decoded rest of the --query value after the
// cursor: parsed is the :reload arguments up to the cursor, whole all of
// them. The value is the token at the same index in both; the rest is what
// whole's token adds to parsed's (the --query= of that form included in
// both). It is "" when the token does not continue (a line that does not
// tokenize the same way past the cursor).
func queryValueTail(parsed partialArgs, whole string) string {
	all := splitInteractiveArgsPartial(whole)
	tokens := append(slices.Clone(all.tokens), all.current)
	i := len(parsed.tokens)
	if i >= len(tokens) || !strings.HasPrefix(tokens[i], parsed.current) {
		return ""
	}
	return tokens[i][len(parsed.current):]
}

// decodedReplacement returns what replaces word: the only candidate, or the
// common prefix of several (cut back to a whole UTF-8 character) when it is
// longer than word; otherwise "" and the candidates to list. All of it is
// decoded query text, escaped by the caller only after the prefix is taken,
// so that a prefix can never end inside an escape sequence.
func decodedReplacement(word string, candidates []string) (string, []string) {
	switch len(candidates) {
	case 0:
		return "", nil
	case 1:
		return candidates[0], nil
	}
	prefix := runePrefix(commonPrefix(candidates))
	if len(prefix) <= len(word) {
		return "", candidates
	}
	return prefix, nil
}

// runePrefix cuts s back to its longest prefix that ends at a UTF-8
// character boundary, so a common prefix of candidates sharing only the
// first byte of a multi-byte character inserts no broken byte.
func runePrefix(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// queryValueOf returns the --query value the cursor is in (decoded so far)
// and the offset in parsed.current where it starts (flagValueOf).
func queryValueOf(parsed partialArgs) (string, int, bool) {
	return flagValueOf(parsed, queryFlagNames)
}
