package mapr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

const (
	invalidQuery  string = "invalid query: "
	unexpectedEnd string = "unexpected end of query"
)

// ErrEmptyQuery indicates that no mapreduce query was provided.
var ErrEmptyQuery = errors.New("query cannot be empty")

// Outfile represents the output file of a mapreduce query.
type Outfile struct {
	FilePath   string
	AppendMode bool
}

// String returns the string representation of Outfile.
func (o *Outfile) String() string {
	return fmt.Sprintf("Outfile(FilePath:%v,AppendMode:%v)", o.FilePath, o.AppendMode)
}

// Query represents a parsed mapr query.
type Query struct {
	Select       []selectCondition
	Table        string
	Where        []whereCondition
	Set          []setCondition
	GroupBy      []string
	OrderBy      string
	ReverseOrder bool
	GroupKey     string
	Interval     time.Duration
	Limit        int
	Outfile      *Outfile
	RawQuery     string
	tokens       []token
	LogFormat    string
	logger       logging.Logger
}

// String returns the string representation of Query.
func (q *Query) String() string {
	return fmt.Sprintf("Query(Select:%v,Table:%s,Where:%v,Set:%vGroupBy:%v,"+
		"GroupKey:%s,OrderBy:%v,ReverseOrder:%v,Interval:%v,Limit:%d,Outfile:%s,"+
		"RawQuery:%s,tokens:%v,LogFormat:%s)",
		q.Select,
		q.Table,
		q.Where,
		q.Set,
		q.GroupBy,
		q.GroupKey,
		q.OrderBy,
		q.ReverseOrder,
		q.Interval,
		q.Limit,
		q.Outfile,
		q.RawQuery,
		q.tokens,
		q.LogFormat)
}

// NewQuery returns a new mapreduce query.
func NewQuery(queryStr string, logger logging.Logger) (*Query, error) {
	if queryStr == "" {
		return nil, ErrEmptyQuery
	}
	tokens := tokenize(queryStr)
	q := Query{
		RawQuery: queryStr,
		tokens:   tokens,
		Interval: time.Second * 5,
		Limit:    -1,
		logger:   logging.OrNop(logger),
	}

	// Parse the query tokens to populate all fields including LogFormat and Table.
	if err := q.parse(tokens); err != nil {
		return nil, err
	}

	// If the log format is CSV and no explicit FROM table was provided, default
	// the table to "." so that all lines are processed without file filtering.
	// This check must run after parse() because LogFormat is only populated
	// once parseTokens() has processed the "logformat" keyword.
	if q.LogFormat == "csv" && q.Table == "" {
		q.Table = "."
	}

	return &q, nil
}

// HasOutfile returns true if query result will be written to a CSV output file.
func (q *Query) HasOutfile() bool {
	return q.Outfile != nil
}

// Has is a helper to determine whether a query contains a substring
func (q *Query) Has(what string) bool {
	return strings.Contains(q.RawQuery, what)
}

func (q *Query) parse(tokens []token) error {
	if _, err := q.parseTokens(tokens); err != nil {
		return fmt.Errorf("failed to parse query tokens: %w", err)
	}

	if len(q.Select) < 1 {
		return errors.New(invalidQuery + "expected at least one field in 'select' " +
			"clause but got none")
	}

	if len(q.GroupBy) == 0 {
		field := q.Select[0].Field
		q.GroupBy = append(q.GroupBy, field)
	}

	if q.OrderBy != "" {
		var orderFieldIsValid bool
		for _, sc := range q.Select {
			if q.OrderBy == sc.FieldStorage {
				orderFieldIsValid = true
				break
			}
		}
		if !orderFieldIsValid {
			return errors.New(invalidQuery + fmt.Sprintf("cannot '(r)order by' '%s',"+
				"must be present in 'select' clause", q.OrderBy))
		}
	}

	return nil
}

// parseTokens parses the clauses of tokens. Every clause starts with a bare
// clause keyword (token.isKeyword, the rule tokensConsume ends a clause at
// and AnalyzePartial starts one at), so only the first token can be
// anything else: it is rejected by firstWordError, also when it is a quoted
// or backticked keyword ("select" used to start the select clause here
// while AnalyzePartial marked it unknown).
func (q *Query) parseTokens(tokens []token) ([]token, error) {
	for len(tokens) > 0 {
		if !tokens[0].isKeyword() {
			return tokens, firstWordError(tokens[0])
		}
		var err error
		switch strings.ToLower(tokens[0].str) {
		case "select":
			tokens, err = q.parseSelectClause(tokens[1:])
		case "from":
			tokens, err = q.parseFromClause(tokens[1:])
		case "where":
			tokens, err = q.parseWhereClause(tokens[1:])
		case "set":
			tokens, err = q.parseSetClause(tokens[1:])
		case "group":
			tokens, err = q.parseGroupClause(tokens[1:])
		case "rorder":
			tokens, err = q.parseOrderClause(tokens[1:], true)
		case "order":
			tokens, err = q.parseOrderClause(tokens[1:], false)
		case "interval":
			tokens, err = q.parseIntervalClause(tokens[1:])
		case "limit":
			tokens, err = q.parseLimitClause(tokens[1:])
		case "outfile":
			tokens, err = q.parseOutfileClause(tokens[1:])
		case "logformat":
			tokens, err = q.parseLogFormatClause(tokens[1:])
		default:
			// A keyword of the keywords table without a clause parser.
			return tokens, errors.New(invalidQuery + "unexpected keyword " + tokens[0].str)
		}
		if err != nil {
			return tokens, err
		}
	}

	return tokens, nil
}

// firstWordError rejects the word t in front of the first clause, which is
// no bare clause keyword. A quoted word is named without its quotes, like
// the other "unexpected word" errors; a backticked one keeps its backticks.
func firstWordError(t token) error {
	return fmt.Errorf("%sunexpected word %q, a query starts with a clause keyword "+
		"such as select or from (quoted or escaped keywords are no clause keywords)",
		invalidQuery, t.str)
}

func (q *Query) parseSelectClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if err := rejectStrayBy("select", found); err != nil {
		return remaining, err
	}
	var err error
	q.Select, err = makeSelectConditions(found)
	return remaining, err
}

// strayWordError rejects the word extra that follows the value(s) of a
// clause taking a fixed number of values (from, order, rorder, interval,
// limit, outfile, logformat; see parseOrderClause for the direction word
// of order and rorder).
// Such a word used to be ignored, so a half typed or misspelled clause
// keyword ("limit 10 gro") was applied as if it was not there.
// AnalyzePartial marks such a word TokenUnknown.
func strayWordError(clause string, extra token) error {
	return fmt.Errorf("%sunexpected word %q after the value of the '%s' clause, "+
		"which takes no further words (misspelled clause keyword?)", invalidQuery, extra.str, clause)
}

// rejectStrayBy rejects a bare by in the list of the select or group
// clause; tokens are the list after tokensConsume. In those lists every
// word is a field, as fields may be separated by blanks instead of commas,
// so a misspelled clause keyword such as "gropu by x" would select or group
// by the fields gropu, by and x. A field named by is written `by`.
func rejectStrayBy(clause string, tokens []token) error {
	for _, t := range tokens {
		if isBareWord(t, "by") {
			return fmt.Errorf("%sunexpected word %q in the '%s' clause: misspelled "+
				"'group by', 'order by' or 'rorder by'? A field named by is written `by`",
				invalidQuery, t.str, clause)
		}
	}
	return nil
}

func (q *Query) parseFromClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	switch len(found) {
	case 0:
		return remaining, errors.New(invalidQuery + "expected table name after 'from'")
	case 1:
		if found[0].str == "" {
			return remaining, errors.New(invalidQuery + "expected table name after 'from'")
		}
		q.Table = strings.ToUpper(found[0].str)
		return remaining, nil
	default:
		return remaining, strayWordError("from", found[1])
	}
}

func (q *Query) parseWhereClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	var err error
	q.Where, err = makeWhereConditions(found)
	return remaining, err
}

func (q *Query) parseSetClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	var err error
	q.Set, err = makeSetConditions(found)
	return remaining, err
}

func (q *Query) parseGroupClause(tokens []token) ([]token, error) {
	tokens = tokensConsumeOptional(tokens, "by")
	if len(tokens) < 1 {
		return tokens, errors.New(invalidQuery + unexpectedEnd)
	}
	remaining, found := tokensConsume(tokens)
	if err := rejectStrayBy("group", found); err != nil {
		return remaining, err
	}
	fields := make([]string, 0, len(found))
	for _, field := range found {
		if field.str == "" {
			return remaining, errors.New(invalidQuery + "expected field name in 'group' clause")
		}
		fields = append(fields, field.str)
	}
	q.GroupBy = fields
	q.GroupKey = strings.Join(fields, ",")
	return remaining, nil
}

// parseOrderClause parses "order [by] ORDERFIELD [desc]" and "rorder [by]
// ORDERFIELD [asc]". order sorts descending and rorder ascending already,
// so the matching SQL direction word (orderDirection; desc is used by
// existing queries) is accepted right after the field and changes nothing.
// Any other further word is rejected (orderWordError), also one after the
// direction word, which is then named; such words used to be ignored, so
// "order by x asc" silently sorted descending.
func (q *Query) parseOrderClause(tokens []token, reverse bool) ([]token, error) {
	tokens = tokensConsumeOptional(tokens, "by")
	if len(tokens) < 1 {
		return tokens, errors.New(invalidQuery + unexpectedEnd)
	}
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 || found[0].str == "" {
		return remaining, errors.New(invalidQuery + unexpectedEnd)
	}
	extra := found[1:]
	if len(extra) > 0 && isBareWord(extra[0], orderDirection(reverse)) {
		extra = extra[1:]
	}
	if len(extra) > 0 {
		return remaining, orderWordError(reverse, extra[0])
	}
	q.OrderBy = found[0].str
	if reverse {
		q.ReverseOrder = true
	}
	return remaining, nil
}

// orderDirection returns the SQL direction word that may follow the field
// of an order (desc) or rorder (asc) clause, the direction it sorts in
// anyway. The parser and AnalyzePartial share it.
func orderDirection(reverse bool) string {
	if reverse {
		return "asc"
	}
	return "desc"
}

// isBareWord tells whether t is the bare, unescaped word (any case), such
// as a by in a select or group list (rejectStrayBy) or the direction word
// after the field of an order clause. The parser and AnalyzePartial share
// this rule; the escaped `by` or `desc` is a field.
func isBareWord(t token, word string) bool {
	return t.isBareword && !t.quotesStripped && strings.EqualFold(t.str, word)
}

// orderWordError rejects the word extra after the field (and direction
// word) of an order or rorder clause, explaining the sort directions for
// asc and desc.
func orderWordError(reverse bool, extra token) error {
	clause := "order"
	if reverse {
		clause = "rorder"
	}
	if isBareWord(extra, "asc") || isBareWord(extra, "desc") {
		return fmt.Errorf("%sunexpected word %q after the value of the '%s' clause: "+
			"'order by FIELD [desc]' sorts descending, 'rorder by FIELD [asc]' ascending",
			invalidQuery, extra.str, clause)
	}
	return strayWordError(clause, extra)
}

// parseIntervalClause parses "interval NUMBER". An interval without its
// number used to keep the default of 5 seconds silently; it is rejected now,
// as a missing limit is.
func (q *Query) parseIntervalClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, errors.New(invalidQuery + "expected number after 'interval'")
	}
	if len(found) > 1 {
		return remaining, strayWordError("interval", found[1])
	}
	interval, err := strconv.Atoi(found[0].str)
	if err != nil {
		return remaining, errors.New(invalidQuery + err.Error())
	}
	if !validInterval(interval) {
		return remaining, errors.New(invalidQuery + "interval out of range: " + found[0].str)
	}
	q.Interval = time.Second * time.Duration(interval)
	return remaining, nil
}

// validInterval tells whether an interval in seconds is accepted. A negative
// interval made dserver and client fall back to different defaults, and a
// huge one overflowed time.Duration into a garbage value. AnalyzePartial
// uses the same rule.
func validInterval(seconds int) bool {
	return seconds >= 0 && int64(seconds) <= int64(math.MaxInt64/time.Second)
}

func (q *Query) parseLimitClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, errors.New(invalidQuery + unexpectedEnd)
	}
	if len(found) > 1 {
		return remaining, strayWordError("limit", found[1])
	}
	limit, err := strconv.Atoi(found[0].str)
	if err != nil {
		return remaining, errors.New(invalidQuery + err.Error())
	}
	// -1 is the internal "no limit clause" value: a negative limit would
	// otherwise silently lift the default row cap of the terminal output.
	if limit < 0 {
		return remaining, errors.New(invalidQuery + "limit must not be negative: " + found[0].str)
	}
	q.Limit = limit
	return remaining, nil
}

// parseOutfileClause parses "outfile [append] PATH". A lone append is the
// path "append". A word after the path is rejected with its name, and so is
// a path starting with "append:" (see rejectAppendColonPath).
func (q *Query) parseOutfileClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, errors.New(invalidQuery + "expected file path in 'outfile' clause")
	}
	appendMode := len(found) > 1 && found[0].str == "append"
	if appendMode {
		found = found[1:]
	}
	if len(found) > 1 {
		return remaining, strayWordError("outfile", found[1])
	}
	if found[0].str == "" {
		return remaining, errors.New(invalidQuery + "expected file path in 'outfile' clause")
	}
	if err := rejectAppendColonPath(found[0].str); err != nil {
		return remaining, err
	}
	q.Outfile = &Outfile{FilePath: found[0].str, AppendMode: appendMode}
	return remaining, nil
}

// appendColonPrefix starts an outfile path that looks like an append mode.
const appendColonPrefix = "append:"

// rejectAppendColonPath rejects an outfile path starting with "append:".
// No release had an "append:PATH" syntax, but it reads like one: such a
// query (an integration test used it for years, a scheduled job may) wrote
// a new file literally named "append:PATH" on every run and never appended
// to PATH. A file really named so is still written as "./append:PATH".
func rejectAppendColonPath(path string) error {
	if !strings.HasPrefix(path, appendColonPrefix) {
		return nil
	}
	return fmt.Errorf("%soutfile path %q starts with %q: to append to a file write "+
		"'outfile append PATH', for a file named so write './%s'",
		invalidQuery, path, appendColonPrefix, path)
}

func (q *Query) parseLogFormatClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 || found[0].str == "" {
		return remaining, errors.New(invalidQuery + unexpectedEnd)
	}
	if len(found) > 1 {
		return remaining, strayWordError("logformat", found[1])
	}
	q.LogFormat = found[0].str
	return remaining, nil
}
