package mapr

import (
	"errors"
	"fmt"
	"strings"
)

// AggregateOperation is to specify the aggregate operation type.
type AggregateOperation int

// Aggregate operation types
const (
	UndefAggregateOperation AggregateOperation = iota
	Count                   AggregateOperation = iota
	Sum                     AggregateOperation = iota
	Min                     AggregateOperation = iota
	Max                     AggregateOperation = iota
	Last                    AggregateOperation = iota
	Avg                     AggregateOperation = iota
	Len                     AggregateOperation = iota
)

// Represents a parsed "select" clause, used by mapr.Query.
type selectCondition struct {
	Field        string
	FieldStorage string
	Operation    AggregateOperation
}

func (sc selectCondition) String() string {
	return fmt.Sprintf("selectCondition(Field:%s,FieldStorage:%s,Operation:%v)",
		sc.Field,
		sc.FieldStorage,
		sc.Operation)
}

func makeSelectConditions(tokens []token) ([]selectCondition, error) {
	var sel []selectCondition
	// Parse select aggregation, e.g. sum(foo)
	parse := func(token token) (selectCondition, error) {
		var sc selectCondition
		tokenStr := strings.ToLower(token.str)

		if !strings.Contains(tokenStr, "(") && !strings.Contains(tokenStr, ")") {
			sc.Field = tokenStr
			sc.FieldStorage = tokenStr
			sc.Operation = Last
			return sc, nil
		}

		a := strings.Split(tokenStr, "(")
		if len(a) != 2 {
			return sc, errors.New(invalidQuery + "Can't parse 'select' aggregation: " +
				token.str)
		}
		agg := a[0] // Aggregation, e.g. 'sum'

		b := strings.Split(a[1], ")")
		if len(b) != 2 {
			return sc, errors.New(invalidQuery + "Can't parse 'select' field name " +
				"from aggregation: " + token.str)
		}
		sc.Field = b[0]            // Field name, e.g. 'foo'
		sc.FieldStorage = tokenStr // e.g. 'sum(foo)'

		switch agg {
		case "count":
			sc.Operation = Count
		case "sum":
			sc.Operation = Sum
		case "min":
			sc.Operation = Min
		case "max":
			sc.Operation = Max
		case "last":
			sc.Operation = Last
		case "avg":
			sc.Operation = Avg
		case "len":
			sc.Operation = Len
		default:
			return sc, errors.New(invalidQuery +
				"Unknown aggregation in 'select' clause: " + agg)
		}
		return sc, nil
	}

	for _, token := range tokens {
		sc, err := parse(token)
		if err != nil {
			return nil, err
		}
		sel = append(sel, sc)
	}
	return sel, nil
}
