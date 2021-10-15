package mapr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/mapr/funcs"
)

// Represent a parsed "set" clause, used by mapr.Query
type setCondition struct {
	lString string

	rType   fieldType
	rString string
	rFloat  float64

	// For now only text functions are supported.
	// Maybe in the future we can have typed functions too
	// so that a float input/output is possible.
	functionStack funcs.FunctionStack
}

func (sc *setCondition) String() string {
	return fmt.Sprintf("setCondition(lString:%s,rString:%s,rType:%s,functionStack:%v)",
		sc.lString, sc.rString, sc.rType.String(), sc.functionStack)
}

func makeSetConditions(tokens []token) (set []setCondition, err error) {
	parse := func(tokens []token) (setCondition, []token, error) {
		var sc setCondition
		if len(tokens) < 3 {
			return sc, nil, errors.New(invalidQuery + "Not enough arguments in 'set' clause")
		}

		setOp := strings.ToLower(tokens[1].str)
		switch setOp {
		case "=":
		default:
			return sc, nil, errors.New(invalidQuery + "Unknown operation in 'set' " +
				"clause: " + setOp)
		}

		if !tokens[0].isBareword {
			return sc, nil, errors.New(invalidQuery + "Expected bareword at 'set' " +
				"clause's lValue: " + tokens[0].str)
		}
		sc.lString = tokens[0].str
		if !strings.HasPrefix(sc.lString, "$") {
			return sc, nil, errors.New(invalidQuery + "Expected field variable name " +
				"(starting with $) at 'set' clause's lValue: " + tokens[0].str)
		}
		sc.rType = Field
		rString := tokens[2].str

		// Seems like a function call?
		if strings.HasSuffix(rString, ")") {
			functionStack, functionArg, err := funcs.NewFunctionStack(tokens[2].str)
			if err != nil {
				return sc, nil, err
			}
			sc.functionStack = functionStack
			sc.rType = FunctionStack
			sc.rString = functionArg
			return sc, tokens[3:], nil
		}

		sc.rString = rString
		if f, err := strconv.ParseFloat(sc.rString, 64); err == nil {
			sc.rFloat = f
			sc.rType = Float
		} else {
			sc.rType = Field
		}
		return sc, tokens[3:], nil
	}

	for len(tokens) > 0 {
		var sc setCondition
		var err error

		sc, tokens, err = parse(tokens)
		if err != nil {
			return nil, err
		}
		set = append(set, sc)
		tokens = tokensConsumeOptional(tokens, ",")
	}
	return
}
