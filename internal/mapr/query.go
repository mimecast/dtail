package mapr

import (
	"errors"
	"fmt"
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

// HasOutfile returns true if query result will be written to a CVS output file.
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

func (q *Query) parseTokens(tokens []token) ([]token, error) {
	for len(tokens) > 0 {
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
			return tokens, errors.New(invalidQuery + "unexpected keyword " + tokens[0].str)
		}
		if err != nil {
			return tokens, err
		}
	}

	return tokens, nil
}

func (q *Query) parseSelectClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	var err error
	q.Select, err = makeSelectConditions(found)
	return remaining, err
}

func (q *Query) parseFromClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	switch len(found) {
	case 0:
		return remaining, errors.New(invalidQuery + "expected table name after 'from'")
	case 1:
		q.Table = strings.ToUpper(found[0].str)
		return remaining, nil
	default:
		return remaining, errors.New(invalidQuery + "expected only one table name after 'from'")
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
	remaining, fields := tokensConsumeStr(tokens)
	q.GroupBy = fields
	q.GroupKey = strings.Join(fields, ",")
	return remaining, nil
}

func (q *Query) parseOrderClause(tokens []token, reverse bool) ([]token, error) {
	tokens = tokensConsumeOptional(tokens, "by")
	if len(tokens) < 1 {
		return tokens, errors.New(invalidQuery + unexpectedEnd)
	}
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, errors.New(invalidQuery + unexpectedEnd)
	}
	q.OrderBy = found[0].str
	if reverse {
		q.ReverseOrder = true
	}
	return remaining, nil
}

func (q *Query) parseIntervalClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, nil
	}
	interval, err := strconv.Atoi(found[0].str)
	if err != nil {
		return remaining, errors.New(invalidQuery + err.Error())
	}
	q.Interval = time.Second * time.Duration(interval)
	return remaining, nil
}

func (q *Query) parseLimitClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, errors.New(invalidQuery + unexpectedEnd)
	}
	limit, err := strconv.Atoi(found[0].str)
	if err != nil {
		return remaining, errors.New(invalidQuery + err.Error())
	}
	q.Limit = limit
	return remaining, nil
}

func (q *Query) parseOutfileClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	switch len(found) {
	case 1:
		q.Outfile = &Outfile{FilePath: found[0].str, AppendMode: false}
	case 2:
		if found[0].str != "append" {
			return remaining, errors.New(invalidQuery + invalidQuery)
		}
		q.Outfile = &Outfile{FilePath: found[1].str, AppendMode: true}
	default:
		return remaining, errors.New(invalidQuery + invalidQuery)
	}
	return remaining, nil
}

func (q *Query) parseLogFormatClause(tokens []token) ([]token, error) {
	remaining, found := tokensConsume(tokens)
	if len(found) == 0 {
		return remaining, errors.New(invalidQuery + unexpectedEnd)
	}
	q.LogFormat = found[0].str
	return remaining, nil
}
