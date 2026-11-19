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

// One can argue that this function is too large (as reported by automatic tools such
// as SonarQube). However, refactoring this method into several smaller ones would make
// the code as a matter of fact less readable. Also, I want to have at least one issue
// reported in SonarQube, just to make sure that SonarQube still works ;-)
func (q *Query) parseTokens(tokens []token) ([]token, error) {
	var err error
	var found []token

	for len(tokens) > 0 {
		switch strings.ToLower(tokens[0].str) {
		case "select":
			tokens, found = tokensConsume(tokens[1:])
			q.Select, err = makeSelectConditions(found)
			if err != nil {
				return tokens, err
			}
		case "from":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return tokens, errors.New(invalidQuery + "expected table name after 'from'")
			}
			if len(found) > 1 {
				return tokens, errors.New(invalidQuery + "expected only one table name after 'from'")
			}
			q.Table = strings.ToUpper(found[0].str)
		case "where":
			tokens, found = tokensConsume(tokens[1:])
			if q.Where, err = makeWhereConditions(found); err != nil {
				return tokens, err
			}
		case "set":
			tokens, found = tokensConsume(tokens[1:])
			if q.Set, err = makeSetConditions(found); err != nil {
				return tokens, err
			}
		case "group":
			tokens = tokensConsumeOptional(tokens[1:], "by")
			if len(tokens) < 1 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			tokens, q.GroupBy = tokensConsumeStr(tokens)
			q.GroupKey = strings.Join(q.GroupBy, ",")
		case "rorder":
			tokens = tokensConsumeOptional(tokens[1:], "by")
			if len(tokens) < 1 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			tokens, found = tokensConsume(tokens)
			if len(found) == 0 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			q.OrderBy = found[0].str
			q.ReverseOrder = true
		case "order":
			tokens = tokensConsumeOptional(tokens[1:], "by")
			if len(tokens) < 1 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			tokens, found = tokensConsume(tokens)
			if len(found) == 0 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			q.OrderBy = found[0].str
		case "interval":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) > 0 {
				i, err := strconv.Atoi(found[0].str)
				if err != nil {
					return tokens, errors.New(invalidQuery + err.Error())
				}
				q.Interval = time.Second * time.Duration(i)
			}
		case "limit":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			i, err := strconv.Atoi(found[0].str)
			if err != nil {
				return tokens, errors.New(invalidQuery + err.Error())
			}
			q.Limit = i
		case "outfile":
			tokens, found = tokensConsume(tokens[1:])
			switch len(found) {
			case 1:
				q.Outfile = &Outfile{FilePath: found[0].str, AppendMode: false}
			case 2:
				if found[0].str == "append" {
					q.Outfile = &Outfile{FilePath: found[1].str, AppendMode: true}
				} else {
					return tokens, errors.New(invalidQuery + invalidQuery)
				}
			default:
				return tokens, errors.New(invalidQuery + invalidQuery)
			}
		case "logformat":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return tokens, errors.New(invalidQuery + unexpectedEnd)
			}
			q.LogFormat = found[0].str
		default:
			return tokens, errors.New(invalidQuery + "unexpected keyword " + tokens[0].str)
		}
	}

	return tokens, nil
}
