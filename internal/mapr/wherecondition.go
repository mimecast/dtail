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

func makeWhereConditions(tokens []token) (where []whereCondition, err error) {

	// Helper to parse a where condition.
	parse := func(tokens []token) (whereCondition, []token, error) {
		var wc whereCondition
		if len(tokens) < 3 {
			return wc, nil, errors.New(invalidQuery + "Not enough arguments in 'where' clause")
		}

		whereOp := strings.ToLower(tokens[1].str)
		switch whereOp {
		case "==":
			wc.Operation = FloatEq
		case "!=":
			wc.Operation = FloatNe
		case "<":
			wc.Operation = FloatLt
		case "<=":
			wc.Operation = FloatLe
		case "=<":
			wc.Operation = FloatLe
		case ">":
			wc.Operation = FloatGt
		case ">=":
			wc.Operation = FloatGe
		case "=>":
			wc.Operation = FloatGe
		case "eq":
			wc.Operation = StringEq
		case "ne":
			wc.Operation = StringNe
		case "contains":
			wc.Operation = StringContains
		case "lacks":
			fallthrough
		case "ncontains":
			wc.Operation = StringNotContains
		case "hasprefix":
			wc.Operation = StringHasPrefix
		case "nhasprefix":
			wc.Operation = StringNotHasPrefix
		case "hassuffix":
			wc.Operation = StringHasSuffix
		case "nhassuffix":
			wc.Operation = StringNotHasSuffix
		default:
			return wc, nil, errors.New(invalidQuery +
				"Unknown operation in 'where' clause: " + whereOp)
		}

		var err error
		tokens, err = wc.fill(tokens)
		return wc, tokens, err
	}

	// Consume all where conditions.
	for len(tokens) > 0 {
		var wc whereCondition
		var err error
		wc, tokens, err = parse(tokens)
		if err != nil {
			return nil, err
		}
		where = append(where, wc)
		tokens = tokensConsumeOptional(tokens, "and")
	}
	return
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
