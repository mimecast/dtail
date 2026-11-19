package mapr

import (
	"slices"
	"strings"

	"github.com/mimecast/dtail/internal/mapr/funcs"
)

// TokenKind classifies a piece of a query for highlighting.
type TokenKind int

// The kinds of query pieces AnalyzePartial reports.
const (
	// TokenUnknown is a token the parser rejects, such as a word before the
	// first clause keyword, an unknown aggregation or operator, a word after
	// the value of a one-value clause (a second table name, "limit 10 gro")
	// or a bare by in a select or group list.
	TokenUnknown TokenKind = iota
	// TokenKeyword is a clause keyword or one of the words by, and, append,
	// or the direction word after the field of an order (desc) or rorder
	// (asc) clause.
	TokenKeyword
	// TokenAggregation is the name of a select aggregation, e.g. count.
	TokenAggregation
	// TokenField is a field name (a bareword).
	TokenField
	// TokenVariable is a field name starting with $.
	TokenVariable
	// TokenOperator is a where operator or the = of a set condition.
	TokenOperator
	// TokenString is a quoted string (quotes included) or an outfile path.
	TokenString
	// TokenNumber is a number literal.
	TokenNumber
	// TokenFunction is the name of a set function, e.g. md5sum.
	TokenFunction
	// TokenTable is the table of a from clause.
	TokenTable
	// TokenLogFormat is the name of a logformat clause.
	TokenLogFormat
)

var tokenKindNames = [...]string{"unknown", "keyword", "aggregation", "field",
	"variable", "operator", "string", "number", "function", "table", "logformat"}

// Expect is a set of the kinds of words that may follow at the cursor.
type Expect uint32

// The kinds of words AnalyzePartial expects. Where several are expected, all
// of them may be written there; a word of another kind makes the query fail
// to parse or changes its meaning. Inside an open double quote
// (Partial.InQuote) only the kinds a quoted token is read as are expected
// (quotedExpect: values, outfile paths, tables, log formats, order items,
// numbers, and fields in select and group), written there as they are and
// closed by the quote; where a quoted word is only a string literal, such
// as in a set condition, nothing is expected.
const (
	// ExpectClauseKeyword: a clause keyword (Keywords). It is expected only
	// where the current clause may end.
	ExpectClauseKeyword Expect = 1 << iota
	// ExpectField: a field or $variable.
	ExpectField
	// ExpectAggregation: an aggregation of a select field, e.g. count(...).
	ExpectAggregation
	// ExpectAggregationField: the field inside AGG( (Partial.Aggregation).
	ExpectAggregationField
	// ExpectWhereOperator: an operator of WhereOperators.
	ExpectWhereOperator
	// ExpectAnd: the word and, joining two where conditions.
	ExpectAnd
	// ExpectValue: a value for Partial.ValueField compared with
	// Partial.ValueOperator (quoted for a string operator).
	ExpectValue
	// ExpectSetFunction: a set function of funcs.Names, e.g. md5sum(...).
	ExpectSetFunction
	// ExpectTable: a table name.
	ExpectTable
	// ExpectLogFormat: a log format name.
	ExpectLogFormat
	// ExpectBy: the optional by of group, order and rorder.
	ExpectBy
	// ExpectAppend: the optional append of outfile.
	ExpectAppend
	// ExpectNumber: a whole number (interval, limit).
	ExpectNumber
	// ExpectSelectItem: a select item typed so far (Partial.SelectItems),
	// for order and rorder.
	ExpectSelectItem
	// ExpectSetVariable: the $variable a set condition assigns to.
	ExpectSetVariable
	// ExpectAssign: the = of a set condition.
	ExpectAssign
	// ExpectOutfilePath: the quoted path of an outfile clause.
	ExpectOutfilePath
)

var expectNames = [...]string{"clause-keyword", "field", "aggregation",
	"aggregation-field", "where-operator", "and", "value", "set-function",
	"table", "logformat", "by", "append", "number", "select-item",
	"set-variable", "assign", "outfile-path"}

// PartialToken is a piece of a query: query[Start:End] is its source text.
// A token of the tokenizer can give several pieces: count(foo) gives the
// aggregation count and the field foo (the parentheses belong to none), and
// md5sum(maskdigits($a)) two functions and a variable.
type PartialToken struct {
	Start int
	End   int
	Text  string
	Kind  TokenKind
}

// Partial is the analysis of a query typed up to the cursor, which is at
// its end. See AnalyzePartial.
type Partial struct {
	// Query is the analysed query.
	Query string
	// Tokens are the pieces of the query with their kinds, in query order.
	Tokens []PartialToken
	// Clause is the keyword (lower case) of the clause the cursor is in,
	// empty before the first clause keyword.
	Clause string
	// Word is the word under the cursor that a candidate replaces, always
	// Query[WordStart:]. It is empty after a blank, a comma or a closing
	// quote; inside a quote, backticks or AGG( it starts after the opening
	// quote, backtick or parenthesis.
	Word      string
	WordStart int
	// AfterComma tells that a comma is among the blanks between the token
	// the word is in (or the cursor, when no word is started) and the token
	// before it. The parser reads a comma as a blank, but a completer can
	// tell a finished item ("select count(x) ") from the start of another
	// one ("select count(x), ").
	AfterComma bool
	// InQuote tells that the word follows an unclosed double quote.
	InQuote bool
	// InBacktick tells that the word follows an opening backtick that is
	// not closed yet (an escaped field name, see doc/querylanguage.md).
	InBacktick bool
	// InAggregation tells that the word is the field inside the select
	// aggregation Aggregation, e.g. fo in "select count(fo".
	InAggregation bool
	Aggregation   string
	// Expect is the set of kinds of words that may be written at WordStart.
	Expect Expect
	// ValueField and ValueOperator are the field and operator of the where
	// condition whose value is expected (ExpectValue). ValueOperator.Name is
	// empty when the operator is unknown.
	ValueField    string
	ValueOperator WhereOperator
	// SelectItems are the select items typed before the word, in the form
	// order by compares (selectCondition.FieldStorage, e.g. count(foo)),
	// from the last select clause, as in the parser.
	SelectItems []string
	// Table (upper case, as the parser stores it) and LogFormat are the
	// from table and the log format typed before the word, empty if none.
	Table     string
	LogFormat string
}

// AnalyzePartial analyses a query typed up to the cursor, which is at the
// end of query, for completion and highlighting. It splits the query into
// tokens with the parser's tokenizer rules (scanQuery mirrors tokenize: a
// double quote has no escape, commas are blanks, backticks are stripped only
// around a whole token, keywords are case insensitive barewords) and follows
// the clauses with the parser's per-clause rules (Query.parseTokens), as far
// as a prefix allows. It never fails: a token the parser rejects on its own
// (an unknown aggregation, operator or function, a malformed select item or
// function call, a number out of range, a word before the first clause, an
// extra table name, an outfile path starting with "append:") is of kind
// TokenUnknown, so a token of another kind is
// one the parser accepts at its place. Rules that span tokens or clauses are
// not checked: a missing select clause, a where or set condition cut short,
// or an order item that is not selected. An aggregation still being typed
// (count(fo is rejected by the parser too, and so is TokenUnknown until it
// is closed. A word the cursor is still in (a bareword or an open
// quote at the very end) is the Word; the expectation is that of its
// position, not of what follows it.
func AnalyzePartial(query string) Partial {
	return analyze(query, true)
}

// AnalyzeQuery analyses a whole query, such as the text of a query on both
// sides of the cursor: as AnalyzePartial, but no word is under the cursor,
// so every token is followed before the state is taken. Clause, Expect,
// SelectItems, Table and LogFormat describe the query up to its end (a
// table typed last, "from STATS", is the Table); Word is empty and
// WordStart is len(query), also after an open quote.
func AnalyzeQuery(query string) Partial {
	return analyze(query, false)
}

// analyze walks the tokens of query. With cursorWord, a bareword or open
// quote reaching the end of query is the word under the cursor: the state
// is taken before it (AnalyzePartial); otherwise after the last token.
func analyze(query string, cursorWord bool) Partial {
	p := Partial{Query: query, WordStart: len(query)}
	scanned := scanQuery(query)
	last := len(scanned) - 1
	inWord := cursorWord && last >= 0 && isWordInProgress(scanned[last], len(query))
	var w partialWalker
	for i, st := range scanned {
		if inWord && i == last {
			w.snapshot(&p)
		}
		w.step(st)
	}
	if !inWord {
		w.snapshot(&p)
	}
	p.Tokens = w.spans
	for i := range p.Tokens {
		p.Tokens[i].Text = query[p.Tokens[i].Start:p.Tokens[i].End]
	}
	before, wordToken := scanned, len(query)
	if inWord {
		setWord(&p, scanned[last])
		before, wordToken = scanned[:last], scanned[last].start
	}
	p.AfterComma = strings.Contains(query[gapStart(before):wordToken], ",")
	p.Word = query[p.WordStart:]
	return p
}

// gapStart returns where the blanks after the tokens before end: the end
// of the last of them, 0 for none.
func gapStart(before []scannedToken) int {
	if len(before) == 0 {
		return 0
	}
	return before[len(before)-1].end
}

// EffectiveLogFormat returns the log format the query typed so far uses, by
// the rule of Query.EffectiveLogFormat, so that a completer offers the
// fields of the right parser.
func (p Partial) EffectiveLogFormat(configuredDefault string) string {
	q := Query{Table: p.Table, LogFormat: p.LogFormat}
	return q.EffectiveLogFormat(configuredDefault)
}

// String returns the name of the token kind.
func (k TokenKind) String() string {
	if k < 0 || int(k) >= len(tokenKindNames) {
		return "invalid"
	}
	return tokenKindNames[k]
}

// Has tells whether e contains any kind of kinds.
func (e Expect) Has(kinds Expect) bool {
	return e&kinds != 0
}

// String returns the names of the kinds in e, joined by |.
func (e Expect) String() string {
	var names []string
	for i, name := range expectNames {
		if e.Has(1 << i) {
			names = append(names, name)
		}
	}
	return strings.Join(names, "|")
}

// isWordInProgress tells whether the cursor (at queryLen) is still in the
// token st: a bareword or an unclosed quoted string reaching the end. After
// a closing quote the next word starts.
func isWordInProgress(st scannedToken, queryLen int) bool {
	return st.end == queryLen && (st.isBareword || st.open)
}

// setWord sets the word under the cursor to the token st it is in, and
// narrows the expectation of its position to what the word can still be.
func setWord(p *Partial, st scannedToken) {
	p.WordStart = st.start
	switch {
	case st.open:
		p.InQuote = true
		p.WordStart++
		p.Expect &= quotedExpect(p.Clause)
	case st.str[0] == '`' && !stripBackticks(st.token).quotesStripped:
		// An escaped name is taken literally: no keyword, aggregation,
		// function or by.
		p.InBacktick = true
		p.WordStart++
		p.Expect &^= ExpectClauseKeyword | ExpectAggregation | ExpectSetFunction | ExpectBy
	default:
		setCallWord(p, st)
	}
}

// quotedWordExpect are the kinds of words written inside double quotes: a
// quoted token is a string, which the parser takes as a where value, an
// outfile path, a table, a log format, an order item or a number (interval,
// limit), all by its text. In select and group a quoted token is a field as
// well (quotedExpect).
const quotedWordExpect = ExpectValue | ExpectOutfilePath | ExpectTable |
	ExpectLogFormat | ExpectSelectItem | ExpectNumber

// quotedExpect returns the kinds of words that can be written inside an
// open double quote in clause. The others can not: a quoted token is no
// clause keyword; a quoted field or function call of a where or set
// condition is a string literal (no kind stands for the free text of a set
// value, so nothing is expected there); a quoted $variable to set is
// rejected. The
// parser would take some others quoted (a where operator, the = of set, an
// aggregation such as select "count(x)", by, and, append), but nobody
// quotes them, so they are not expected there either.
func quotedExpect(clause string) Expect {
	if clause == "select" || clause == "group" {
		return quotedWordExpect | ExpectField
	}
	return quotedWordExpect
}

// setCallWord moves the word after the opening parenthesis of an
// aggregation (select count(fo) or a set function (md5sum(maskdigits(fo)
// that is not closed yet. An escaped token (`count(`) is a literal name, and
// so is a call of a name that is no set function (foo(), which the parser
// rejects: no function context is set up after it.
func setCallWord(p *Partial, st scannedToken) {
	open := strings.LastIndexByte(st.str, '(')
	if open < 0 || strings.Contains(st.str[open:], ")") ||
		stripBackticks(st.token).quotesStripped {
		return
	}
	switch {
	case p.Clause == "select" && p.Expect.Has(ExpectAggregation):
		// Only a valid aggregation name opens an aggregation (not count)().
		if _, err := parseAggregateOperation(st.str[:open]); err != nil ||
			strings.Count(st.str, "(") != 1 {
			return
		}
		p.InAggregation = true
		p.Aggregation = st.str[:open]
		p.Expect = ExpectAggregationField
	case p.Expect.Has(ExpectSetFunction):
		if !isSetFunctionChain(st.str[:open]) {
			return
		}
		p.Expect = ExpectSetFunction | ExpectField
	default:
		return
	}
	p.WordStart = st.start + open + 1
}

// isSetFunctionChain tells whether calls, the text before the last opening
// parenthesis of a set value such as md5sum(maskdigits, names only set
// functions, each followed by its opening parenthesis.
func isSetFunctionChain(calls string) bool {
	names := funcs.Names()
	for _, name := range strings.Split(calls, "(") {
		if !slices.Contains(names, name) {
			return false
		}
	}
	return true
}
