package mapr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	invalidQuery  string = "Invalid query: "
	unexpectedEnd string = "Unexpected end of query"
)

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
	Outfile      string
	RawQuery     string
	tokens       []token
	LogFormat    string
}

func (q Query) String() string {
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
func NewQuery(queryStr string) (*Query, error) {
	if queryStr == "" {
		return nil, nil
	}
	tokens := tokenize(queryStr)
	q := Query{
		RawQuery: queryStr,
		tokens:   tokens,
		Interval: time.Second * 5,
		Limit:    -1,
	}
	return &q, q.parse(tokens)
}

// HasOutfile returns true if query result will be written to a CVS output file.
func (q *Query) HasOutfile() bool {
	return q.Outfile != ""
}

// Has is a helper to determine whether a query contains a substring
func (q *Query) Has(what string) bool {
	return strings.Contains(q.RawQuery, what)
}

func (q *Query) parse(tokens []token) error {
	var found []token
	var err error

	for tokens != nil && len(tokens) > 0 {
		switch strings.ToLower(tokens[0].str) {
		case "select":
			tokens, found = tokensConsume(tokens[1:])
			q.Select, err = makeSelectConditions(found)
			if err != nil {
				return err
			}
		case "from":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return errors.New(invalidQuery + "expected table name after 'from'")
			}
			if len(found) > 1 {
				return errors.New(invalidQuery + "expected only one table name after 'from'")
			}
			q.Table = strings.ToUpper(found[0].str)
		case "where":
			tokens, found = tokensConsume(tokens[1:])
			if q.Where, err = makeWhereConditions(found); err != nil {
				return err
			}
		case "set":
			tokens, found = tokensConsume(tokens[1:])
			if q.Set, err = makeSetConditions(found); err != nil {
				return err
			}
		case "group":
			tokens = tokensConsumeOptional(tokens[1:], "by")
			if tokens == nil || len(tokens) < 1 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			tokens, q.GroupBy = tokensConsumeStr(tokens)
			q.GroupKey = strings.Join(q.GroupBy, ",")
		case "rorder":
			tokens = tokensConsumeOptional(tokens[1:], "by")
			if tokens == nil || len(tokens) < 1 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			tokens, found = tokensConsume(tokens)
			if len(found) == 0 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			q.OrderBy = found[0].str
			q.ReverseOrder = true
		case "order":
			tokens = tokensConsumeOptional(tokens[1:], "by")
			if tokens == nil || len(tokens) < 1 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			tokens, found = tokensConsume(tokens)
			if len(found) == 0 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			q.OrderBy = found[0].str
		case "interval":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) > 0 {
				i, err := strconv.Atoi(found[0].str)
				if err != nil {
					return errors.New(invalidQuery + err.Error())
				}
				q.Interval = time.Second * time.Duration(i)
			}
		case "limit":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			i, err := strconv.Atoi(found[0].str)
			if err != nil {
				return errors.New(invalidQuery + err.Error())
			}
			q.Limit = i
		case "outfile":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			q.Outfile = found[0].str
		case "logformat":
			tokens, found = tokensConsume(tokens[1:])
			if len(found) == 0 {
				return errors.New(invalidQuery + unexpectedEnd)
			}
			q.LogFormat = found[0].str
		default:
			return errors.New(invalidQuery + "Unexpected keyword " + tokens[0].str)
		}
	}

	if len(q.Select) < 1 {
		return errors.New(invalidQuery + "Expected at least one field in 'select' " +
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
			return errors.New(invalidQuery + fmt.Sprintf("Can not '(r)order by' '%s',"+
				"must be present in 'select' clause", q.OrderBy))
		}
	}

	return nil
}
