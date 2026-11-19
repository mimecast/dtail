package clients

import (
	"errors"
	"fmt"
	"io"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// queryInvertReason says why --invert does not apply to a mapreduce query
// (see applyQueryRegex).
const queryInvertReason = "a mapreduce query (--query) selects the lines of its table itself"

// ignoredInvertWarning tells the user that dtail dropped the --invert of its
// command line because a mapreduce query was given.
const ignoredInvertWarning = "WARN: Ignoring --invert: " + queryInvertReason

// errReloadQueryInvert rejects `:reload --invert` in an interactive query
// session (see validateInteractiveReloadArgs). The reload is rejected rather
// than applied without the --invert, so the TTY says why and the current
// workload keeps running, as for any other rejected reload.
var errReloadQueryInvert = errors.New("--invert is not supported: " + queryInvertReason)

func maprRegexForQuery(query *mapr.Query) string {
	if query == nil {
		return "."
	}

	switch query.Table {
	case "", ".":
		return "."
	case "*":
		return "\\|MAPREDUCE:\\|"
	default:
		return fmt.Sprintf("\\|MAPREDUCE:%s\\|", query.Table)
	}
}

// applyQueryRegex sets the line filter of a mapreduce query on args: the
// regex is derived from the query's table (maprRegexForQuery) and replaces any
// --regex, and RegexInvert is cleared. The filter only selects the lines the
// query aggregates, so inverting it has no sensible meaning: for a named table
// it would feed every other line into the aggregation, and for the tables ""
// and "." (every line) only the empty lines. dtail therefore accepts --invert
// together with --query and ignores it, with a warning (warnIgnoredInvert).
// An interactive :reload of a query session rejects --invert instead
// (errReloadQueryInvert), so it is never silently dropped there either.
//
// The read commands built from a query session spec ignore RegexInvert
// anyway (session.Spec.queryCommands), so clearing it here is not needed for
// a current dserver. It is kept so that the client's own args and the
// regex_invert field of a SESSION START/UPDATE spec say what is done, and
// because an older dserver builds the read commands of such a spec itself
// and would invert the filter.
func applyQueryRegex(args *config.Args, query *mapr.Query) {
	args.RegexStr = maprRegexForQuery(query)
	args.RegexInvert = false
}

// warnIgnoredInvert writes ignoredInvertWarning to w when args asks to invert
// the regex of a mapreduce query, which applyQueryRegex ignores.
func warnIgnoredInvert(w io.Writer, args config.Args, logger logging.Logger) {
	if !args.RegexInvert {
		return
	}
	if _, err := fmt.Fprintln(w, ignoredInvertWarning); err != nil {
		logger.Debug("Unable to write ignored invert warning", err)
	}
}
