package dlog

import (
	"fmt"
	"strings"
)

type level int

const (
	FATAL   level = iota
	ERROR   level = iota
	WARN    level = iota
	INFO    level = iota
	DEFAULT level = iota
	VERBOSE level = iota
	DEBUG   level = iota
	DEVEL   level = iota
	TRACE   level = iota
	ALL     level = iota
)

var allLevels = []level{
	FATAL,
	ERROR,
	WARN,
	INFO,
	DEFAULT,
	VERBOSE,
	DEBUG,
	DEVEL,
	TRACE,
	ALL,
}

func newLevel(l string) level {
	switch strings.ToUpper(l) {
	case "FATAL":
		return FATAL
	case "ERROR":
		return ERROR
	case "WARN":
		return WARN
	case "INFO":
		return INFO
	case "":
		fallthrough
	case "DEFAULT":
		return DEFAULT
	case "VERBOSE":
		return VERBOSE
	case "DEBUG":
		return DEBUG
	case "DEVEL":
		return DEVEL
	case "TRACE":
		return TRACE
	case "ALL":
		return ALL
	}
	panic(fmt.Sprintf("Unknown log level %s, must be one of: %v", l, allLevels))
}

func (l level) String() string {
	switch l {
	case FATAL:
		return "FATAL"
	case ERROR:
		return "ERROR"
	case WARN:
		return "WARN"
	case INFO:
		return "INFO"
	case DEFAULT:
		return "DEFAULT"
	case VERBOSE:
		return "VERBOSE"
	case DEBUG:
		return "DEBUG"
	case DEVEL:
		return "DEVEL"
	case TRACE:
		return "TRACE"
	case ALL:
		return "ALL"
	}

	panic("Unknown log level " + fmt.Sprintf("%d", l))
}
