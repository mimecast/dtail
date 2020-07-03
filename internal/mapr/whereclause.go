package mapr

import (
	"strconv"

	"github.com/mimecast/dtail/internal/io/logger"
)

// WhereClause interprets the where clause of the mapreduce query.
func (q *Query) WhereClause(fields map[string]string) bool {
	for _, wc := range q.Where {
		var ok bool

		if wc.Operation > FloatOperation {
			var lValue, rValue float64
			if lValue, ok = whereClauseFloatValue(fields, wc.lString, wc.lFloat, wc.lType); !ok {
				return false
			}
			if rValue, ok = whereClauseFloatValue(fields, wc.rString, wc.rFloat, wc.rType); !ok {
				return false
			}
			if ok = wc.floatClause(lValue, rValue); !ok {
				return false
			}
			continue
		}

		var lValue, rValue string
		if lValue, ok = whereClauseStringValue(fields, wc.lString, wc.lType); !ok {
			return false
		}
		if rValue, ok = whereClauseStringValue(fields, wc.rString, wc.rType); !ok {
			return false
		}
		if ok = wc.stringClause(lValue, rValue); !ok {
			return false
		}
	}

	return true
}

func whereClauseFloatValue(fields map[string]string, str string, float float64, t fieldType) (float64, bool) {
	switch t {
	case Float:
		return float, true
	case Field:
		value, ok := fields[str]
		if !ok {
			return 0, false
		}
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		logger.Error("Unexpected argument in 'where' clause", str, float, t)
		return 0, false
	}
}

func whereClauseStringValue(fields map[string]string, str string, t fieldType) (string, bool) {
	switch t {
	case Field:
		value, ok := fields[str]
		if !ok {
			return str, false
		}
		return value, true
	case String:
		return str, true
	default:
		logger.Error("Unexpected argument in 'where' clause", str, t)
		return str, false
	}
}
