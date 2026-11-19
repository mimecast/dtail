package dlog

import (
	"fmt"
	"strings"
)

type level int

// Available log levels.
const (
	None    level = iota
	Fatal   level = iota
	Error   level = iota
	Warn    level = iota
	Info    level = iota
	Default level = iota
	Verbose level = iota
	Debug   level = iota
	Devel   level = iota
	Trace   level = iota
	All     level = iota
)

var allLevels = []level{Fatal, Error, Warn, Info, Default, Verbose, Debug,
	Devel, Trace, All}

func newLevel(l string) (level, error) {
	switch strings.ToLower(l) {
	case "none":
		return None, nil
	case "fatal":
		return Fatal, nil
	case "error":
		return Error, nil
	case "warn":
		return Warn, nil
	case "info":
		return Info, nil
	case "":
		fallthrough
	case "default":
		return Default, nil
	case "verbose":
		return Verbose, nil
	case "debug":
		return Debug, nil
	case "devel":
		return Devel, nil
	case "trace":
		return Trace, nil
	case "all":
		return All, nil
	}
	return None, fmt.Errorf("unknown log level %q, must be one of: %v", l, allLevels)
}

func (l level) String() string {
	switch l {
	case None:
		return "NONE"
	case Fatal:
		return "FATAL"
	case Error:
		return "ERROR"
	case Warn:
		return "WARN"
	case Info:
		return "INFO"
	case Default:
		return "DEFAULT"
	case Verbose:
		return "VERBOSE"
	case Debug:
		return "DEBUG"
	case Devel:
		return "DEVEL"
	case Trace:
		return "TRACE"
	case All:
		return "ALL"
	}
	panic("Unknown log level " + fmt.Sprintf("%d", l))
}
