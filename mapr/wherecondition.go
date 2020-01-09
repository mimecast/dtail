package mapr

import (
	"dtail/logger"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// QueryOperation determines the mapreduce operation.
type QueryOperation int

// The possible mapreduce operation.s
const (
	UndefQueryOperation QueryOperation = iota
	StringEq            QueryOperation = iota
	StringNe            QueryOperation = iota
	StringContains      QueryOperation = iota
	FloatOperation      QueryOperation = iota
	FloatEq             QueryOperation = iota
	FloatNe             QueryOperation = iota
	FloatLt             QueryOperation = iota
	FloatLe             QueryOperation = iota
	FloatGt             QueryOperation = iota
	FloatGe             QueryOperation = iota
)

type whereType int

// The possible field types.
const (
	UndefWhereType whereType = iota
	Field          whereType = iota
	String         whereType = iota
	Float          whereType = iota
)

func (w whereType) String() string {
	switch w {
	case Field:
		return fmt.Sprintf("Field")
	case String:
		return fmt.Sprintf("String")
	case Float:
		return fmt.Sprintf("Float")
	default:
		return fmt.Sprintf("UndefWhereType")
	}
}

// Represent a parsed "where" clause, used by mapr.Query
type whereCondition struct {
	lString string
	lFloat  float64
	lType   whereType

	Operation QueryOperation

	rString string
	rFloat  float64
	rType   whereType
}

func (wc *whereCondition) String() string {
	return fmt.Sprintf("whereCondition(Operation:%v,lString:%s,lFloat:%v,lType:%s,rString:%s,rFloat:%v,rType:%s)",
		wc.Operation, wc.lString, wc.lFloat, wc.lType.String(), wc.rString, wc.rFloat, wc.rType.String())
}

func makeWhereConditions(tokens []token) (where []whereCondition, err error) {
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
		default:
			return wc, nil, errors.New(invalidQuery + "Unknown operation in 'where' clause: " + whereOp)
		}

		wc.lString = tokens[0].str
		wc.rString = tokens[2].str

		if wc.Operation > FloatOperation {
			if !tokens[0].isBareword {
				return wc, nil, errors.New(invalidQuery + "Expected bareword at 'where' clause's lValue: " + tokens[0].str)
			}
			if f, err := strconv.ParseFloat(wc.lString, 64); err == nil {
				wc.lFloat = f
				wc.lType = Float
			} else {
				wc.lType = Field
			}

			if !tokens[2].isBareword {
				return wc, nil, errors.New(invalidQuery + "Expected bareword at 'where' clause's rValue: " + tokens[2].str)
			}
			if f, err := strconv.ParseFloat(wc.rString, 64); err == nil {
				wc.rFloat = f
				wc.rType = Float
			} else {
				wc.rType = Field
			}
			return wc, tokens[3:], nil
		}

		if tokens[0].isBareword {
			wc.lType = Field
		} else {
			wc.lType = String
		}
		if tokens[2].isBareword {
			wc.rType = Field
		} else {
			wc.rType = String
		}

		return wc, tokens[3:], nil
	}

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

func (wc *whereCondition) floatClause(lValue float64, rValue float64) bool {
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

func (wc *whereCondition) stringClause(lValue string, rValue string) bool {
	switch wc.Operation {
	case StringEq:
		return lValue == rValue
	case StringNe:
		return lValue != rValue
	case StringContains:
		return strings.Contains(lValue, rValue)
	default:
		logger.Error("Unknown string operation", lValue, wc.Operation, rValue)
	}
	return false
}
