package funcs

import (
	"fmt"
	"strings"
)

// CallbackFunc is a function which can be executed by the mapreduce engine
type CallbackFunc func(text string) string

// Function embeddes the function name to the callback function
type Function struct {
	// Name of the callback function
	Name string
	// The Go-callback function to call for this DTail function.
	call CallbackFunc
}

// FunctionStack is a list of functions stacked each other
type FunctionStack []Function

// NewFunctionStack parses the input string, e.g. foo(bar("arg")) and returns a corresponding function stack.
func NewFunctionStack(in string) (FunctionStack, string, error) {
	var fs FunctionStack

	getCallback := func(name string) (CallbackFunc, error) {
		var cb CallbackFunc

		switch name {
		case "md5sum":
			return Md5Sum, nil
		case "maskdigits":
			return MaskDigits, nil
		default:
			return cb, fmt.Errorf("unknown function '%s'", name)
		}
	}

	aux := in
	for strings.HasSuffix(aux, ")") {
		index := strings.Index(aux, "(")
		if index <= 0 {
			return fs, "", fmt.Errorf("unable to parse function '%s' at '%s'", in, aux)
		}
		name := aux[0:index]

		call, err := getCallback(name)
		if err != nil {
			return fs, "", err
		}
		fs = append(fs, Function{name, call})
		aux = aux[index+1 : len(aux)-1]
	}

	return fs, aux, nil
}

// Call the function stack.
func (fs FunctionStack) Call(str string) string {
	for i := len(fs) - 1; i >= 0; i-- {
		//logger.Debug("Call", fs[i].Name, str)
		str = fs[i].call(str)
		//logger.Debug("Call.result", fs[i].Name, str)
	}

	return str
}
