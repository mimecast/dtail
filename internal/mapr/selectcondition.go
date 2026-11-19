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
	Percentage              AggregateOperation = iota
	Percentile              AggregateOperation = iota
)

// aggregationTable lists every aggregation of a select field (e.g. the sum of
// sum(foo)) with its operation, in the order of doc/querylanguage.md. The
// parser (parseAggregateOperation) and AggregationNames, which the
// interactive query completer reads, share this one table so that they
// cannot drift apart. It is only read while a query is parsed.
var aggregationTable = [...]struct {
	name      string
	operation AggregateOperation
}{
	{"count", Count},
	{"sum", Sum},
	{"min", Min},
	{"max", Max},
	{"avg", Avg},
	{"last", Last},
	{"len", Len},
	{"percentage", Percentage},
	{"percentile", Percentile},
}

// Represents a parsed "select" clause, used by mapr.Query.
type selectCondition struct {
	Field        string
	FieldStorage string
	Operation    AggregateOperation
}

// AggregationNames returns the names of the select aggregations, such as
// count or sum, in the order of the parser's table. The names are case
// sensitive. The result is a new slice, which the caller may change.
func AggregationNames() []string {
	names := make([]string, len(aggregationTable))
	for i, entry := range aggregationTable {
		names[i] = entry.name
	}
	return names
}

func (sc selectCondition) String() string {
	return fmt.Sprintf("selectCondition(Field:%s,FieldStorage:%s,Operation:%v)",
		sc.Field,
		sc.FieldStorage,
		sc.Operation)
}

// makeSelectConditions parses the fields of a 'select' clause.
//
// Every select field must have its own storage key (FieldStorage, the field
// as written, e.g. 'count(x)'): the aggregate sets, the wire format and the
// result columns are all keyed by it. Two fields sharing one would add each
// line to the same value once per field, and so would the client aggregation
// of a server's value and the merge into the global result (count(x),count(x)
// came out 8 times too high, percentage() too low). A backtick-quoted field
// named like an aggregation (`count(x)`) would send its value under the same
// wire key as that aggregation. Such a query is rejected instead, by the
// client before it connects and by dserver (which also rejects clients of
// another protocol version), so it never produces wrong numbers (see
// doc/querylanguage.md).
func makeSelectConditions(tokens []token) ([]selectCondition, error) {
	var sel []selectCondition
	seen := make(map[string]selectCondition, len(tokens))
	for _, token := range tokens {
		sc, err := parseSelectCondition(token)
		if err != nil {
			return nil, err
		}
		if first, ok := seen[sc.FieldStorage]; ok {
			return nil, duplicateSelectError(first, sc)
		}
		seen[sc.FieldStorage] = sc
		sel = append(sel, sc)
	}
	return sel, nil
}

// duplicateSelectError explains why the select field dup is rejected: the
// earlier select field first has the same storage key. Either both are the
// same field or aggregation, or one is a backtick-quoted field named like the
// other one's aggregation.
func duplicateSelectError(first, dup selectCondition) error {
	if first.Field == dup.Field && first.Operation == dup.Operation {
		return errors.New(invalidQuery + "'select' clause has '" + dup.FieldStorage +
			"' more than once, select each field or aggregation only once")
	}
	return errors.New(invalidQuery + "'select' clause has both the field `" +
		dup.FieldStorage + "` and the aggregation " + dup.FieldStorage +
		", which would share one result column, select only one of them")
}

// parseSelectCondition parses one select field: a plain field (aggregated
// with last), or an aggregation of a field such as sum(foo).
func parseSelectCondition(token token) (selectCondition, error) {
	var sc selectCondition
	if token.str == "" {
		return sc, errors.New(invalidQuery + "expected field name in 'select' clause")
	}

	// With quotes stripped: We got a quoted select expression, e.g.
	// "select `count($foo)` ...", which will literaly look for field
	// "count($foo)" without performing the count aggregation.
	if token.quotesStripped || (!strings.Contains(token.str, "(") && !strings.Contains(token.str, ")")) {
		sc.Field = token.str
		sc.FieldStorage = token.str
		sc.Operation = Last
		return sc, nil
	}

	a := strings.Split(token.str, "(")
	if len(a) != 2 {
		return sc, errors.New(invalidQuery + "Can't parse 'select' aggregation: " +
			token.str)
	}
	agg := a[0] // Aggregation, e.g. 'sum'

	b := strings.Split(a[1], ")")
	// Nothing may follow the closing parenthesis: "count(foo)bar" used to be
	// accepted as count(foo) under the column name "count(foo)bar".
	if len(b) != 2 || b[1] != "" {
		return sc, errors.New(invalidQuery + "Can't parse 'select' field name " +
			"from aggregation: " + token.str)
	}
	sc.Field = b[0]             // Field name, e.g. 'foo'
	sc.FieldStorage = token.str // e.g. 'sum(foo)'

	var err error
	sc.Operation, err = parseAggregateOperation(agg)
	return sc, err
}

// parseAggregateOperation maps the name of a select aggregation to its
// operation, looked up in aggregationTable. The name is case sensitive.
func parseAggregateOperation(agg string) (AggregateOperation, error) {
	for _, entry := range aggregationTable {
		if entry.name == agg {
			return entry.operation, nil
		}
	}
	return UndefAggregateOperation,
		errors.New(invalidQuery + "Unknown aggregation in 'select' clause: " + agg)
}
