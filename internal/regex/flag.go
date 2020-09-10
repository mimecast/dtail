package regex

import "fmt"

type Flag int

const (
	// Undefined flag set
	Undefined Flag = iota
	// Default is the default regex mode (positive matching)
	Default Flag = iota
	// Invert inverts the regex
	Invert Flag = iota
	// Noop means no regex matching enabled, all defaults to true
	Noop Flag = iota
)

func NewFlag(str string) (Flag, error) {
	switch str {
	case "default":
		return Default, nil
	case "invert":
		return Invert, nil
	case "noop":
		return Noop, nil
	default:
		return Undefined, fmt.Errorf("unknown regex flag '%s', setting to 'undefined'", str)
	}
}

func (f Flag) String() string {
	switch f {
	case Default:
		return "default"
	case Invert:
		return "invert"
	case Noop:
		return "noop"
	default:
		return "undefined"
	}
}
