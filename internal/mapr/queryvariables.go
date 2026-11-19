package mapr

import (
	"sort"
	"strings"
)

// EffectiveLogFormat returns the log format parser name that will be used to
// evaluate the query. It centralises the parser-selection rule so that both the
// server (resolveParserName) and the client-side plan-time diagnostics agree on
// which parser a query targets.
//
// An explicit `logformat` clause always wins. Without one, a query lacking a
// `from TABLE` clause downgrades to the "generic" parser (which exposes only the
// common $-variables and no dynamic key=value fields). A query with a `from`
// clause uses configuredDefault, or "default" when configuredDefault is empty.
func (q *Query) EffectiveLogFormat(configuredDefault string) string {
	if configuredDefault == "" {
		configuredDefault = "default"
	}
	if q == nil {
		return configuredDefault
	}
	if q.LogFormat != "" {
		return q.LogFormat
	}
	if q.Table == "" {
		return "generic"
	}
	return configuredDefault
}

// ReferencedVariables returns the sorted, de-duplicated set of $-prefixed field
// variables the query references in its select, where, group-by and set clauses.
//
// Variables defined by the query itself via the `set` clause (the left-hand
// side names) are excluded, because they are legitimately produced at runtime
// even though no parser populates them. Bare (non-$) field names are dynamic
// key=value fields that legitimately vary per line and are never returned; only
// the reserved $-prefixed names are candidates for the unknown-variable
// diagnostic.
func (q *Query) ReferencedVariables() []string {
	if q == nil {
		return nil
	}

	produced := make(map[string]struct{}, len(q.Set))
	for _, sc := range q.Set {
		produced[sc.lString] = struct{}{}
	}

	referenced := make(map[string]struct{})
	add := func(name string) {
		if !strings.HasPrefix(name, "$") {
			return
		}
		if _, isProduced := produced[name]; isProduced {
			return
		}
		referenced[name] = struct{}{}
	}

	for _, sc := range q.Select {
		add(sc.Field)
	}
	for _, groupBy := range q.GroupBy {
		add(groupBy)
	}
	for _, wc := range q.Where {
		if wc.lType == Field {
			add(wc.lString)
		}
		if wc.rType == Field {
			add(wc.rString)
		}
	}
	for _, sc := range q.Set {
		if sc.rType == Field || sc.rType == FunctionStack {
			add(sc.rString)
		}
	}

	if len(referenced) == 0 {
		return nil
	}
	variables := make([]string, 0, len(referenced))
	for name := range referenced {
		variables = append(variables, name)
	}
	sort.Strings(variables)
	return variables
}
