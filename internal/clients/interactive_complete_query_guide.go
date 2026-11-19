package clients

import (
	"slices"
	"strings"
	"unicode"

	"github.com/mimecast/dtail/internal/mapr"
)

// This file holds the guided part of the query completion: Tab offers one
// step at a time in a fixed order, from -> table -> select -> items ->
// where -> group by -> order by -> limit. The table comes first because it
// scopes the fields, $variables and sample values offered after it: a
// query begun with Tab starts with from, and with its table too when the
// catalog knows only one (completeFromFirst lists several in the same
// Tab). A log format the servers answered for without tables (csv, or
// plain logs only generic parses) starts at select. Clauses already in
// the query are not offered again, rare clauses only once their first
// letters are typed, and any clause order the parser accepts still
// completes when typed by hand.

const (
	// commaItemHint is shown after a finished select item or group by
	// field, where Tab lists only the clauses that may follow.
	commaItemHint = "(, adds another item)"
	// commaConditionHint is shown after a finished where condition.
	commaConditionHint = "(, or and adds another condition)"
	// setAssignmentHint is shown after a finished set assignment.
	setAssignmentHint = "(a $variable adds another assignment)"
	// tableFirstHint is shown where fields are offered while the query
	// names no table but the catalog knows tables of its log format.
	tableFirstHint = "(choose a table first, from <table>: it narrows the fields and $variables to that table)"
	// tableNameHint is shown where a query starts with from, or a table
	// is expected after it, while no server has answered the field probe
	// yet (an empty catalog): the table's name is typed by hand.
	tableNameHint = "(a table name follows from: the servers reported none yet, type it)"
)

// queryClause is a clause keyword and what Tab inserts for it: group,
// order and rorder are inserted with their by.
type queryClause struct {
	keyword, insert string
}

// guidedQueryClauses are the clauses Tab offers where a clause may start,
// in the guided order; rareQueryClauses are offered only when the word
// typed starts like them. Together they are mapr.Keywords (checked by
// TestGuidedClausesCoverKeywords).
var (
	guidedQueryClauses = []queryClause{{"from", "from "}, {"select", "select "}, {"where", "where "},
		{"group", "group by "}, {"order", "order by "}, {"limit", "limit "}}
	rareQueryClauses = []queryClause{{"set", "set "}, {"interval", "interval "}, {"outfile", "outfile "},
		{"logformat", "logformat "}, {"rorder", "rorder by "}}
)

// queryOutline is what the whole query, on both sides of the cursor, tells
// the completion of the word under the cursor.
type queryOutline struct {
	// used are the clause keywords of the query outside the word under
	// the cursor, by clauseKey.
	used map[string]bool
	// table (upper case) and logFormat are the query's from table and log
	// format, the last ones typed before the word or after the cursor (a
	// later clause replaces an earlier one, as in the parser). The word
	// itself is still being typed and does not count.
	table, logFormat string
	// tailWord is the first word after the cursor that is not the rest of
	// the word under it, e.g. by in "group| by x": a lead word (by, append)
	// it already is is not inserted again (withLead, leadCandidates).
	tailWord string
}

// queryGuide is what the guided completion offers for the word under the
// cursor: the kinds of words (a narrowing of mapr.Partial.Expect) and a
// hint line.
type queryGuide struct {
	expect mapr.Expect
	hint   string
}

// newQueryOutline outlines the query partial was made of (the text up to
// the cursor) followed by tail, the text after the cursor.
func newQueryOutline(partial mapr.Partial, tail string) queryOutline {
	cursor := len(partial.Query)
	whole := mapr.AnalyzeQuery(partial.Query + tail)
	outline := queryOutline{used: make(map[string]bool), table: partial.Table, logFormat: partial.LogFormat}
	keywords := mapr.Keywords()
	for _, tok := range whole.Tokens {
		// The word under the cursor starts at WordStart (a keyword is never
		// inside a call, where WordStart is past the token's start).
		outside := tok.Start < partial.WordStart || tok.Start >= cursor
		lower := strings.ToLower(tok.Text)
		if tok.Kind == mapr.TokenKeyword && outside && slices.Contains(keywords, lower) {
			outline.used[clauseKey(lower)] = true
		}
	}
	if lastTokenAfter(whole.Tokens, mapr.TokenTable, cursor) {
		outline.table = whole.Table
	}
	if lastTokenAfter(whole.Tokens, mapr.TokenLogFormat, cursor) {
		outline.logFormat = whole.LogFormat
	}
	outline.tailWord = nextTailWord(tail, partial.Word != "")
	return outline
}

// nextTailWord returns the first bareword of tail, the text after the
// cursor; inWord tells that the cursor is in a word, whose rest (the
// leading non-blanks of tail) is skipped. A quoted word gives "".
func nextTailWord(tail string, inWord bool) string {
	isSeparator := func(r rune) bool { return r == ',' || unicode.IsSpace(r) }
	if inWord {
		tail = strings.TrimLeftFunc(tail, func(r rune) bool { return !isSeparator(r) })
	}
	tail = strings.TrimLeftFunc(tail, isSeparator)
	if end := strings.IndexFunc(tail, func(r rune) bool { return isSeparator(r) || r == '"' }); end >= 0 {
		tail = tail[:end]
	}
	return tail
}

// hasTailLead reports whether the word after the cursor is lead, the
// optional lead word of a clause: by ignoring case (as the parser), append
// as it is.
func (o queryOutline) hasTailLead(lead string) bool {
	if lead == "by" {
		return strings.EqualFold(o.tailWord, lead)
	}
	return o.tailWord == lead
}

// withLead returns what Tab inserts for clause: without its by when the
// word after the cursor is by already ("group" for "gro| by x").
func (o queryOutline) withLead(clause queryClause) string {
	if strings.HasSuffix(clause.insert, " by ") && o.hasTailLead("by") {
		return clause.keyword + " "
	}
	return clause.insert
}

// leadCandidates returns lead followed by a blank, unless the word after
// the cursor is lead already.
func (o queryOutline) leadCandidates(lead string) []string {
	if o.hasTailLead(lead) {
		return nil
	}
	return []string{lead + " "}
}

// guideQuery narrows what may be written at the word of p to the step Tab
// offers:
//   - after a finished select item, where condition or group by field
//     (a clause keyword and another field may follow): with an empty word
//     only what continues the query (the next clauses, and after a
//     condition) and a hint that a comma adds another item; after a comma
//     the items again; a typed word completes to either;
//   - after a finished set assignment with an empty word only the next
//     clauses and a hint that a $variable adds another assignment;
//   - at a value position only values, no fields;
//   - after group, order or rorder without by only by.
func guideQuery(p mapr.Partial) queryGuide {
	e := p.Expect
	continuation := mapr.ExpectClauseKeyword | mapr.ExpectAnd
	switch {
	case e.Has(mapr.ExpectClauseKeyword) && e.Has(mapr.ExpectField):
		switch {
		case p.AfterComma:
			return queryGuide{expect: e &^ continuation}
		case p.Word != "":
			return queryGuide{expect: e}
		case p.Clause == "where":
			return queryGuide{expect: e & continuation, hint: commaConditionHint}
		}
		return queryGuide{expect: e & continuation, hint: commaItemHint}
	case e.Has(mapr.ExpectClauseKeyword) && e.Has(mapr.ExpectSetVariable) && p.Word == "":
		return queryGuide{expect: mapr.ExpectClauseKeyword, hint: setAssignmentHint}
	case e.Has(mapr.ExpectValue):
		return queryGuide{expect: e &^ mapr.ExpectField}
	case e.Has(mapr.ExpectBy) && p.Word == "":
		return queryGuide{expect: mapr.ExpectBy}
	}
	return queryGuide{expect: e}
}

// nextClauses returns what Tab inserts for the clauses that may start at
// a word: with a typed word every clause not used yet (rare ones too),
// matched later; with an empty word only the next step of the guided
// order. tables are the table candidates of the query's log format and
// tableless tells that the catalog knows that format to have none. The
// step is from while neither from nor select is used, unless the format is
// tableless: with its table when tables holds just one ("from STATS "),
// so that one Tab starts the query with both. Else it is select while it
// is missing (a query needs it), else the guided clauses not used yet
// (from only with tables, and with its one table when it is the only one
// left).
func (o queryOutline) nextClauses(word string, tables []string, tableless bool) []string {
	var next []string
	if word != "" {
		for _, clause := range slices.Concat(guidedQueryClauses, rareQueryClauses) {
			if !o.isUsed(clause.keyword) {
				next = append(next, o.withLead(clause))
			}
		}
		return next
	}
	switch {
	case !o.isUsed("select") && !o.isUsed("from") && len(tables) == 1:
		return []string{"from " + tables[0]}
	case !o.isUsed("select") && !o.isUsed("from") && !tableless:
		return []string{"from "}
	case !o.isUsed("select"):
		return []string{"select "}
	}
	for _, clause := range guidedQueryClauses {
		if !o.isUsed(clause.keyword) && (clause.keyword != "from" || len(tables) > 0) {
			next = append(next, o.withLead(clause))
		}
	}
	if slices.Equal(next, []string{"from "}) && len(tables) == 1 {
		// From is the only step left: with its one table, as at the start.
		return []string{"from " + tables[0]}
	}
	return next
}

// isUsed reports whether the clause of keyword is in the query.
func (o queryOutline) isUsed(keyword string) bool {
	return o.used[clauseKey(keyword)]
}

// clauseKey names the clause of a keyword: order and rorder are one
// clause, which either keyword sets (the later replaces the earlier).
func clauseKey(keyword string) string {
	if keyword == "rorder" {
		return "order"
	}
	return keyword
}

// lastTokenAfter reports whether the last token of kind starts at or after
// the cursor, so that it, not one typed before the cursor, sets the
// query's value of that kind.
func lastTokenAfter(tokens []mapr.PartialToken, kind mapr.TokenKind, cursor int) bool {
	for i := len(tokens) - 1; i >= 0; i-- {
		if tokens[i].Kind == kind {
			return tokens[i].Start >= cursor
		}
	}
	return false
}

// fieldHeadings are the headings of a field position, by clause.
var fieldHeadings = map[string]string{"select": "select item", "where": "field to filter on",
	"group": "field to group by", "set": "value to assign"}

// queryHeading returns the heading of a query listing, what is expected at
// the word of p where the guide expects expect: "next" where only what
// continues the query is offered, "field to count" inside count(, "value
// for x" after a where operator, and so on; "" where the group labels say
// it all: before the first clause (its group is labelled "clause",
// queryClauseLabel) and where what continues the query is offered next to
// other words (a typed word after a finished item), which no one heading
// describes.
func queryHeading(p mapr.Partial, expect mapr.Expect) string {
	continuation := mapr.ExpectClauseKeyword | mapr.ExpectAnd | mapr.ExpectBy | mapr.ExpectAppend
	switch {
	case p.Clause == "" || (expect&continuation != 0 && expect&^continuation != 0):
		return ""
	case expect&^continuation == 0:
		return "next"
	case expect.Has(mapr.ExpectAggregationField):
		return aggregationFieldHeading(p.Aggregation)
	case expect.Has(mapr.ExpectValue):
		return "value for " + displayText(p.ValueField)
	case expect.Has(mapr.ExpectWhereOperator):
		return "operator"
	case expect.Has(mapr.ExpectTable):
		return "table"
	case expect.Has(mapr.ExpectLogFormat):
		return "log format"
	case expect.Has(mapr.ExpectSelectItem):
		return "to order by"
	case expect.Has(mapr.ExpectSetVariable):
		return "$variable to set"
	case expect.Has(mapr.ExpectSetFunction) || expect.Has(mapr.ExpectField):
		return fieldHeadings[p.Clause]
	}
	return ""
}

// aggregationFieldHeading returns the heading inside AGG(: "field to
// count" and "field to sum", "field for avg" and so on.
func aggregationFieldHeading(aggregation string) string {
	aggregation = strings.ToLower(aggregation)
	if aggregation == "count" || aggregation == "sum" {
		return "field to " + aggregation
	}
	return "field for " + aggregation
}
