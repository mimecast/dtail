package logformat

import (
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/mapr"
)

// The $-variable sets below enumerate every $-prefixed variable a given parser
// is able to populate. They are the single source of truth for the plan-time
// "unknown $-variable" diagnostic (see PlanVariableWarnings).
//
// Keep them in sync with defaultParser in default.go:
//   - addDefaultFields populates commonVariables for every parser that embeds
//     defaultParser (generic, generickv, csv and default itself).
//   - defaultParser.MakeFields additionally populates defaultOnlyVariables from
//     the positional fields of DTail's own MAPREDUCE log line layout; the
//     lighter parsers (generic/generickv/csv) override MakeFields and therefore
//     do NOT populate these.

// commonVariables are the $-variables set by defaultParser.addDefaultFields and
// are therefore available in every built-in log format.
var commonVariables = map[string]struct{}{
	"$line":       {},
	"$empty":      {},
	"$hostname":   {},
	"$server":     {},
	"$timezone":   {},
	"$timeoffset": {},
}

// defaultOnlyVariables are the extra $-variables that only the "default" parser
// extracts from DTail's MAPREDUCE log lines (see defaultParser.MakeFields).
var defaultOnlyVariables = map[string]struct{}{
	"$severity":   {},
	"$loglevel":   {},
	"$time":       {},
	"$date":       {},
	"$hour":       {},
	"$minute":     {},
	"$second":     {},
	"$pid":        {},
	"$caller":     {},
	"$cpus":       {},
	"$goroutines": {},
	"$cgocalls":   {},
	"$loadavg":    {},
	"$uptime":     {},
}

// knownVariables returns the set of $-variables the named parser can populate,
// and whether that set is enumerable at all. For log formats whose variable set
// cannot be determined statically (proprietary, stub or unknown formats) it
// returns (nil, false) so callers skip the diagnostic entirely rather than emit
// false-positive warnings for variables that might in fact be valid.
func knownVariables(logFormatName string) (map[string]struct{}, bool) {
	switch logFormatName {
	case "default":
		known := make(map[string]struct{}, len(commonVariables)+len(defaultOnlyVariables))
		for name := range commonVariables {
			known[name] = struct{}{}
		}
		for name := range defaultOnlyVariables {
			known[name] = struct{}{}
		}
		return known, true
	case "generic", "generickv", "csv":
		return commonVariables, true
	default:
		return nil, false
	}
}

// PlanVariableWarnings returns one warning per unknown $-variable referenced by
// the query, given the parser selected by logFormatName. A $-variable is
// "unknown" when the selected parser cannot populate it and the query does not
// define it via a `set` clause; at runtime such a variable silently resolves to
// the empty string, collapsing an aggregation into a single empty group with no
// other diagnostic. This is a WARNING and not an error: sparse dynamic fields
// and built-ins like $empty are legitimate, so resolution behaviour is
// unchanged.
//
// No warnings are produced when the parser's variable set is not enumerable
// (see knownVariables); guessing there would risk false positives, which are
// worse than none because they train users to ignore warnings.
func PlanVariableWarnings(query *mapr.Query, logFormatName string) []string {
	if query == nil {
		return nil
	}
	known, enumerable := knownVariables(logFormatName)
	if !enumerable {
		return nil
	}

	var warnings []string
	for _, variable := range query.ReferencedVariables() {
		if _, ok := known[variable]; ok {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"warning: %s is not a known variable for log format %q; "+
				"did you mean bareword %s?",
			variable, logFormatName, strings.TrimPrefix(variable, "$")))
	}
	return warnings
}
