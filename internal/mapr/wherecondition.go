package mapr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/logging"
)

// QueryOperation determines the mapreduce operation.
type QueryOperation int

// The possible mapreduce operation.s
const (
	UndefQueryOperation QueryOperation = iota
	StringEq            QueryOperation = iota
	StringNe            QueryOperation = iota
	StringContains      QueryOperation = iota
	StringNotContains   QueryOperation = iota
	StringHasPrefix     QueryOperation = iota
	StringNotHasPrefix  QueryOperation = iota
	StringHasSuffix     QueryOperation = iota
	StringNotHasSuffix  QueryOperation = iota
	FloatOperation      QueryOperation = iota
	FloatEq             QueryOperation = iota
	FloatNe             QueryOperation = iota
	FloatLt             QueryOperation = iota
	FloatLe             QueryOperation = iota
	FloatGt             QueryOperation = iota
	FloatGe             QueryOperation = iota
)

// OperatorKind tells which values a where operator compares.
type OperatorKind int

// The kinds of where operators.
const (
	// NumericOperator compares two numbers (==, <, ...). Its operands must be
	// barewords: numbers or fields holding numbers.
	NumericOperator OperatorKind = iota
	// StringOperator compares two strings (eq, contains, ...). Its operands
	// may be fields, quoted strings or bare numbers.
	StringOperator
)

// WhereOperator describes one operator of a where condition.
type WhereOperator struct {
	// Name is the operator as written in a query, in lower case.
	Name string
	// Kind tells whether the operator compares numbers or strings.
	Kind OperatorKind
}

// whereOperatorTable lists every operator of a where condition with its
// operation, in the order of doc/querylanguage.md (float operators first).
// The parser (parseWhereOperation) and WhereOperators, which the interactive
// query completer reads, share this one table so that they cannot drift
// apart. It is only read while a query is parsed.
var whereOperatorTable = [...]struct {
	name      string
	operation QueryOperation
}{
	{"==", FloatEq},
	{"!=", FloatNe},
	{"<", FloatLt},
	{"<=", FloatLe},
	{"=<", FloatLe},
	{">", FloatGt},
	{">=", FloatGe},
	{"=>", FloatGe},
	{"eq", StringEq},
	{"ne", StringNe},
	{"contains", StringContains},
	{"ncontains", StringNotContains},
	{"lacks", StringNotContains},
	{"hasprefix", StringHasPrefix},
	{"nhasprefix", StringNotHasPrefix},
	{"hassuffix", StringHasSuffix},
	{"nhassuffix", StringNotHasSuffix},
}

// Represent a parsed "where" clause, used by mapr.Query
type whereCondition struct {
	lType   fieldType
	lString string
	lFloat  float64

	Operation QueryOperation

	rType   fieldType
	rString string
	rFloat  float64
}

func (wc *whereCondition) String() string {
	return fmt.Sprintf("whereCondition(Operation:%v,lString:%s,lFloat:%v,"+
		"lType:%s,rString:%s,rFloat:%v,rType:%s)",
		wc.Operation, wc.lString, wc.lFloat, wc.lType.String(), wc.rString,
		wc.rFloat, wc.rType.String())
}

// WhereOperators returns the operators of a where condition, such as == or
// contains, with their kind, in the order of the parser's table. Aliases
// (=<, =>, lacks) are listed as operators of their own. The parser matches
// them ignoring case. The result is a new slice, which the caller may change.
func WhereOperators() []WhereOperator {
	ops := make([]WhereOperator, len(whereOperatorTable))
	for i, entry := range whereOperatorTable {
		ops[i] = WhereOperator{Name: entry.name, Kind: entry.operation.kind()}
	}
	return ops
}

// makeWhereConditions parses the conditions of a 'where' clause, separated
// by blanks, commas or the optional keyword 'and'.
func makeWhereConditions(tokens []token) (where []whereCondition, err error) {
	for len(tokens) > 0 {
		var wc whereCondition
		wc, tokens, err = parseWhereCondition(tokens)
		if err != nil {
			return nil, err
		}
		where = append(where, wc)
		tokens = tokensConsumeOptional(tokens, "and")
	}
	return where, nil
}

// parseWhereCondition parses the first condition (ARG OPERATOR ARG) of
// tokens and returns it with the tokens that follow it.
func parseWhereCondition(tokens []token) (whereCondition, []token, error) {
	var wc whereCondition
	if len(tokens) < 3 {
		return wc, nil, errors.New(invalidQuery + "Not enough arguments in 'where' clause")
	}
	var err error
	if wc.Operation, err = parseWhereOperation(tokens[1].str); err != nil {
		return wc, nil, err
	}
	tokens, err = wc.fill(tokens)
	return wc, tokens, err
}

// parseWhereOperation maps the operator of a where condition, ignoring case,
// to its operation, looked up in whereOperatorTable. Operators are resolved
// here, once per parsed query: matching lines only switches on the resulting
// QueryOperation (floatClause, stringClause).
func parseWhereOperation(op string) (QueryOperation, error) {
	op = strings.ToLower(op)
	for _, entry := range whereOperatorTable {
		if entry.name == op {
			return entry.operation, nil
		}
	}
	return UndefQueryOperation,
		errors.New(invalidQuery + "Unknown operation in 'where' clause: " + op)
}

// kind tells whether the operation compares numbers or strings: the float
// operations follow the FloatOperation marker in the constant list.
func (op QueryOperation) kind() OperatorKind {
	if op > FloatOperation {
		return NumericOperator
	}
	return StringOperator
}

// Fill a where condition.
func (wc *whereCondition) fill(tokens []token) ([]token, error) {
	wc.lString = tokens[0].str
	wc.rString = tokens[2].str

	if wc.Operation > FloatOperation {
		if !tokens[0].isBareword {
			return nil, errors.New(invalidQuery +
				"Expected bareword at 'where' clause's lValue: " + tokens[0].str)
		}

		if f, err := strconv.ParseFloat(wc.lString, 64); err == nil {
			wc.lFloat = f
			wc.lType = Float
		} else {
			wc.lType = Field
		}

		if !tokens[2].isBareword {
			return nil, errors.New(invalidQuery +
				"Expected bareword at 'where' clause's rValue: " + tokens[2].str)
		}
		if f, err := strconv.ParseFloat(wc.rString, 64); err == nil {
			wc.rFloat = f
			wc.rType = Float
		} else {
			wc.rType = Field
		}
		return tokens[3:], nil
	}

	wc.lType = stringOperandType(tokens[0])
	wc.rType = stringOperandType(tokens[2])

	return tokens[3:], nil
}

// stringOperandType tells what an operand of a string operation is: a quoted
// token is a string, a bareword a field name. A bareword that is a number
// (e.g. the 1 of "where $a eq 1") is a string literal too, as the grammar
// (ARG := FIELD|FLOAT|STRING) allows a number there; looked up as a field it
// would never exist, so the condition matched no line. A field that is named
// like a number can still be given in backticks.
func stringOperandType(t token) fieldType {
	if !t.isBareword {
		return String
	}
	if !t.quotesStripped && isNumericLiteral(t.str) {
		return String
	}
	return Field
}

// isNumericLiteral reports whether str is a plain decimal number. It is
// stricter than strconv.ParseFloat alone, which also accepts names such as
// "inf", "nan" or "infinity" that may well be field names.
func isNumericLiteral(str string) bool {
	if str == "" {
		return false
	}
	switch c := str[0]; {
	case c >= '0' && c <= '9', c == '-', c == '+', c == '.':
	default:
		return false
	}
	f, err := strconv.ParseFloat(str, 64)
	return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

func (wc *whereCondition) floatClause(lValue float64, rValue float64, logger logging.Logger) bool {
	switch wc.Operation {
	case FloatEq:
		return lValue == rValue
	case FloatNe:
		return lValue != rValue
	case FloatLt:
		return lValue < rValue
	case FloatLe:
		return lValue <= rValue
	case FloatGt:
		return lValue > rValue
	case FloatGe:
		return lValue >= rValue
	default:
		logger.Error("Unknown float operation", lValue, wc.Operation, rValue)
	}
	return false
}

func (wc *whereCondition) stringClause(lValue string, rValue string, logger logging.Logger) bool {
	switch wc.Operation {
	case StringEq:
		return lValue == rValue
	case StringNe:
		return lValue != rValue
	case StringContains:
		return strings.Contains(lValue, rValue)
	case StringNotContains:
		return !strings.Contains(lValue, rValue)
	case StringHasPrefix:
		return strings.HasPrefix(lValue, rValue)
	case StringNotHasPrefix:
		return !strings.HasPrefix(lValue, rValue)
	case StringHasSuffix:
		return strings.HasSuffix(lValue, rValue)
	case StringNotHasSuffix:
		return !strings.HasSuffix(lValue, rValue)
	default:
		logger.Error("Unknown string operation", lValue, wc.Operation, rValue)
	}
	return false
}
