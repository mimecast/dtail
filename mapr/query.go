package mapr

import (
	"dtail/logger"
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
	GroupBy      []string
	OrderBy      string
	ReverseOrder bool
	GroupKey     string
	Interval     time.Duration
	Limit        int
	Outfile      string
	RawQuery     string
	tokens       []token
}

func (q Query) String() string {
	return fmt.Sprintf("Query(Select:%v,Table:%s,Where:%v,GroupBy:%v,GroupKey:%s,OrderBy:%v,ReverseOrder:%v,Interval:%v,Limit:%d,Outfile:%s,RawQuery:%s,tokens:%v)",
		q.Select,
		q.Table,
		q.Where,
		q.GroupBy,
		q.GroupKey,
		q.OrderBy,
		q.ReverseOrder,
		q.Interval,
		q.Limit,
		q.Outfile,
		q.RawQuery,
		q.tokens)
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

	err := q.parse(tokens)

	logger.Debug(q)
	return &q, err
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
			if len(found) > 0 {
				q.Table = strings.ToUpper(found[0].str)
			}
		case "where":
			tokens, found = tokensConsume(tokens[1:])
			if q.Where, err = makeWhereConditions(found); err != nil {
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
		default:
			return errors.New(invalidQuery + "Unexpected keyword " + tokens[0].str)
		}
	}

	if q.Table == "" {
		return errors.New(invalidQuery + "Empty table specified in 'from' clause")
	}
	if len(q.Select) < 1 {
		return errors.New(invalidQuery + "Expected at least one field in 'select' clause but got none")
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
			return errors.New(invalidQuery + fmt.Sprintf("Can not '(r)order by' '%s', must be present in 'select' clause", q.OrderBy))
		}
	}

	return nil
}

// WhereClause interprets the where clause of the mapreduce query.
func (q *Query) WhereClause(fields map[string]string) bool {
	floatValue := func(str string, float float64, t whereType) (float64, bool) {
		switch t {
		case Float:
			return float, true
		case Field:
			value, ok := fields[str]
			if !ok {
				return 0, false
			}
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, false
			}
			return f, true
		default:
			logger.Error("Unexpected argument in 'where' clause", str, float, t)
			return 0, false
		}
	}

	stringValue := func(str string, t whereType) (string, bool) {
		switch t {
		case Field:
			value, ok := fields[str]
			if !ok {
				return str, false
			}
			return value, true
		case String:
			return str, true
		default:
			logger.Error("Unexpected argument in 'where' clause", str, t)
			return str, false
		}
	}

	for _, wc := range q.Where {
		var ok bool

		if wc.Operation > FloatOperation {
			var lValue, rValue float64
			if lValue, ok = floatValue(wc.lString, wc.lFloat, wc.lType); !ok {
				return false
			}
			if rValue, ok = floatValue(wc.rString, wc.rFloat, wc.rType); !ok {
				return false
			}
			if ok = wc.floatClause(lValue, rValue); !ok {
				return false
			}
			continue
		}

		var lValue, rValue string
		if lValue, ok = stringValue(wc.lString, wc.lType); !ok {
			return false
		}
		if rValue, ok = stringValue(wc.rString, wc.rType); !ok {
			return false
		}
		if ok = wc.stringClause(lValue, rValue); !ok {
			return false
		}
	}

	return true
}
