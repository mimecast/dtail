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

// functionTable lists every function of a set condition (e.g. the maskdigits
// of "set $foo = maskdigits($bar)") with its callback, in the order of
// doc/querylanguage.md. The parser (lookupCallback) and Names, which the
// interactive query completer reads, share this one table so that they
// cannot drift apart.
var functionTable = [...]Function{
	{Name: "md5sum", call: Md5Sum},
	{Name: "maskdigits", call: MaskDigits},
}

// NewFunctionStack parses the input string, e.g. foo(bar("arg")) and returns
// a corresponding function stack. It returns an error for malformed inputs
// such as unbalanced parentheses (e.g. "foo(", "foo(bar)baz").
func NewFunctionStack(in string) (FunctionStack, string, error) {
	var fs FunctionStack

	aux := in
	for strings.HasSuffix(aux, ")") {
		index := strings.Index(aux, "(")
		if index <= 0 {
			return fs, "", fmt.Errorf("unable to parse function '%s' at '%s'", in, aux)
		}
		name := aux[0:index]

		call, err := lookupCallback(name)
		if err != nil {
			return fs, "", err
		}
		fs = append(fs, Function{name, call})
		// Strip the outer function name and its enclosing parens, leaving
		// only the argument expression for the next iteration.
		aux = aux[index+1 : len(aux)-1]
	}

	// Validate that no unbalanced parentheses remain in the argument string.
	// Inputs like "foo(bar)baz" leave "bar)baz" after stripping, and inputs
	// ending with "(" (no closing ")") are accepted as plain field literals
	// without this check — both produce silently wrong behavior.
	if err := validateParenBalance(aux, in); err != nil {
		return fs, "", err
	}

	return fs, aux, nil
}

// Names returns the names of the functions a set condition can call, in the
// order of the parser's table. The names are case sensitive. The result is a
// new slice, which the caller may change.
func Names() []string {
	names := make([]string, len(functionTable))
	for i, entry := range functionTable {
		names[i] = entry.Name
	}
	return names
}

// lookupCallback maps a function name (case sensitive) to its CallbackFunc
// implementation, looked up in functionTable. It returns an error for
// unrecognised names so callers get a clear message.
func lookupCallback(name string) (CallbackFunc, error) {
	for _, entry := range functionTable {
		if entry.Name == name {
			return entry.call, nil
		}
	}
	return nil, fmt.Errorf("unknown function '%s'", name)
}

// validateParenBalance checks that the remaining argument string contains no
// unbalanced parentheses. A negative depth means a stray ')' was found; a
// non-zero depth after the loop means an unclosed '(' was found. The original
// full expression is included in the error message for context.
func validateParenBalance(aux, original string) error {
	depth := 0
	for _, r := range aux {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth < 0 {
			return fmt.Errorf("malformed function expression %q: unexpected ')' in argument", original)
		}
	}
	if depth != 0 {
		return fmt.Errorf("malformed function expression %q: unclosed '(' in argument", original)
	}
	return nil
}

// Call the function stack.
func (fs FunctionStack) Call(str string) string {
	for i := len(fs) - 1; i >= 0; i-- {
		str = fs[i].call(str)
	}
	return str
}
