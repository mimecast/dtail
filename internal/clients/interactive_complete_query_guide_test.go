package clients

import (
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/omode"
)

var (
	// guideArgs is an interactive dtail following lines, where :query
	// completes the query typed after it.
	guideArgs = config.Args{Mode: omode.TailClient, What: "integrationtests/mapr_testdata.log"}
	// guideCatalog is what a probe of mapr_testdata.log and of a second
	// table finds: STATS lines fill $hostname and $time, OTHER lines
	// $loglevel; the format-wide $variables are the union.
	guideCatalog = catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {
			Tables: map[string]map[string][]string{
				"STATS": {"currentConnections": {"0", "3"}, "lifetimeConnections": {"1"}},
				"OTHER": {"bytes": {"100"}},
			},
			TableVariables: map[string]map[string][]string{
				"STATS": {"$hostname": {"web1"}, "$time": {"1002-071143"}},
				"OTHER": {"$loglevel": {"INFO"}},
			},
			Variables: map[string][]string{"$hostname": {"web1"}, "$time": {"1002-071143"},
				"$loglevel": {"INFO"}},
		}}}
	// statsOnlyCatalog has the one table of mapr_testdata.log.
	statsOnlyCatalog = catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {
			Tables: map[string]map[string][]string{
				"STATS": {"currentConnections": {"0", "3"}, "lifetimeConnections": {"1"}},
			},
			TableVariables: map[string]map[string][]string{
				"STATS": {"$hostname": {"web1"}, "$time": {"1002-071143"}},
			},
			Variables: map[string][]string{"$hostname": {"web1"}, "$time": {"1002-071143"}},
		}}}
)

// genericOnlyCatalog is what a probe of a plain log finds: the default
// format skips every line, only generic, which has no tables, has fields.
var genericOnlyCatalog = catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
	Formats: map[string]catalogFormat{"generic": {
		Tables: map[string]map[string][]string{"": {"$line": {"hello"}}},
	}}}

// guideStep is one Tab press of a guided session: the line typed (the
// cursor at its end, or at the "|" in it), and either the line Tab makes
// of it or the exact candidates it lists, with the hint it shows.
type guideStep struct {
	line, want string
	candidates []string
	hint       string
}

// aggregationWords are the aggregations as the select position offers
// them.
func aggregationWords() []string {
	return withSuffix(mapr.AggregationNames(), "(")
}

func checkGuideSteps(t *testing.T, args config.Args, catalog catalogSnapshot, steps []guideStep) {
	t.Helper()
	for _, step := range steps {
		line, pos := step.line, len(step.line)
		if i := strings.IndexByte(line, '|'); i >= 0 {
			line, pos = line[:i]+line[i+1:], i
		}
		got := completeInteractive(line, pos, args, catalogOf(catalog))
		want := step.want
		if want == "" {
			want = line
		}
		if got.line != want || !slices.Equal(got.candidates, step.candidates) || got.hint != step.hint {
			t.Errorf("%q: line %q candidates %q hint %q,\nwant line %q candidates %q hint %q",
				step.line, got.line, got.candidates, got.hint, want, step.candidates, step.hint)
		}
	}
}

// TestGuidedQueryTargetSession is the target session of the guided query
// builder: one step per Tab, table first (from and the only table in one
// Tab), everything after it scoped to the table.
func TestGuidedQueryTargetSession(t *testing.T) {
	checkGuideSteps(t, guideArgs, statsOnlyCatalog, []guideStep{
		{line: ":query ", want: ":query from STATS "},
		{line: ":query from ", want: ":query from STATS "},
		{line: ":query from STATS ", want: ":query from STATS select "},
		{line: ":query from STATS select ", candidates: append(aggregationWords(),
			"currentConnections ", "lifetimeConnections ", "$hostname ", "$time ")},
		{line: ":query from STATS select count(", candidates: []string{"currentConnections)",
			"lifetimeConnections)", "$hostname)", "$time)"}},
		{line: ":query from STATS select count(currentConnections) ",
			candidates: []string{"where ", "group by ", "order by ", "limit "}, hint: commaItemHint},
		// Right after the ) Tab inserted: the blank, and the same step.
		{line: ":query from STATS select count(currentConnections)",
			want:       ":query from STATS select count(currentConnections) ",
			candidates: []string{"where ", "group by ", "order by ", "limit "}, hint: commaItemHint},
	})
}

// TestGuidedQueryAfterClosingParenthesis checks Tab right after a finished
// item ending in ")" (an aggregation, a set function, an order by item, a
// where value): at the end of the query it inserts the blank and lists the
// next step with its hint, even a single next clause (a comma may follow
// instead); with text after the cursor it inserts nothing, so it neither
// doubles a blank nor splits a token, and only lists. Tab after a typed
// blank still inserts the one next clause. A word ending in ")" that is no
// finished item (rejected by the parser, quoted, an unclosed inner call)
// changes nothing.
func TestGuidedQueryAfterClosingParenthesis(t *testing.T) {
	const q = ":query from STATS select count(currentConnections)"
	const all = q + " where currentConnections > 0 group by $hostname order by count(currentConnections)"
	next := []string{"where ", "group by ", "order by ", "limit "}
	checkGuideSteps(t, guideArgs, statsOnlyCatalog, []guideStep{
		{line: q + " set $x = md5sum($hostname)", want: q + " set $x = md5sum($hostname) ",
			candidates: next, hint: setAssignmentHint},
		{line: q + " where $hostname eq md5sum($time)", want: q + " where $hostname eq md5sum($time) ",
			candidates: []string{"and ", "group by ", "order by ", "limit "}, hint: commaConditionHint},
		{line: q + " order by count(currentConnections)", want: q + " order by count(currentConnections) ",
			candidates: []string{"where ", "group by ", "limit "}},
		{line: all, want: all + " ", candidates: []string{"limit "}},
		{line: all + " ", want: all + " limit "},
		{line: q + "| where currentConnections > 0",
			candidates: []string{"group by ", "order by ", "limit "}, hint: commaItemHint},
		{line: q + "|,sum(lifetimeConnections)", candidates: next, hint: commaItemHint},
		{line: q + "|x", candidates: next, hint: commaItemHint},
		{line: q + " order by count(currentConnections)| limit 3", candidates: []string{"where ", "group by "}},
		{line: all + "| limit 3"},
		{line: ":query from STATS select foo)"},
		{line: ":query from STATS select count(currentConnections))"},
		{line: ":query from STATS select count(currentConnections)("},
		{line: `:query from STATS select "count(currentConnections)`},
		{line: ":query from STATS select currentConnections set $x = md5sum(maskdigits($hostname)"},
	})
	checkGuideSteps(t, queryArgs, statsOnlyCatalog, []guideStep{
		{line: ":reload --query 'from STATS select count(currentConnections)",
			want:       ":reload --query 'from STATS select count(currentConnections) ",
			candidates: next, hint: commaItemHint},
		{line: ":reload --query 'from STATS select count(currentConnections)|'", candidates: next, hint: commaItemHint,
			want: ":reload --query 'from STATS select count(currentConnections) '"},
	})
}

// TestGuidedQueryClauses covers the steps after the select items: the
// comma rule, where conditions (operators only, then values only), group
// by and order by, used clauses never offered again, and the rare clauses
// completing only when typed.
func TestGuidedQueryClauses(t *testing.T) {
	const q = ":query from STATS select count(currentConnections)"
	checkGuideSteps(t, guideArgs, guideCatalog, []guideStep{
		// After a comma the items come again.
		{line: q + ", ", candidates: append(aggregationWords(),
			"currentConnections ", "lifetimeConnections ", "$hostname ", "$time ")},
		{line: q + ",$h", want: q + ",$hostname "},
		// A typed prefix completes a next clause, else an item.
		{line: q + " w", want: q + " where "},
		{line: q + " gr", want: q + " group by "},
		{line: q + " o", candidates: []string{"order by ", "outfile "}},
		{line: q + " or", want: q + " order by "},
		{line: q + " li", candidates: []string{"limit ", "lifetimeConnections "}},
		{line: q + " lif", want: q + " lifetimeConnections "},
		// Rare clauses only when typed.
		{line: q + " se", want: q + " set "},
		{line: q + " int", want: q + " interval "},
		{line: q + " ou", want: q + " outfile "},
		{line: q + " logf", want: q + " logformat "},
		{line: q + " ro", want: q + " rorder by "},
		// where: fields of the table, then operators, then values.
		{line: q + " where ", candidates: []string{"currentConnections ", "lifetimeConnections ",
			"$hostname ", "$time "}},
		{line: q + " where currentConnections == ", candidates: []string{"0 ", "3 "}},
		{line: q + " where $hostname eq ", want: q + ` where $hostname eq "web1" `},
		{line: q + " where currentConnections > 0 ",
			candidates: []string{"and ", "group by ", "order by ", "limit "}, hint: commaConditionHint},
		{line: q + " where currentConnections > 0 and ", candidates: []string{"currentConnections ",
			"lifetimeConnections ", "$hostname ", "$time "}},
		{line: q + " where currentConnections > 0, ", candidates: []string{"currentConnections ",
			"lifetimeConnections ", "$hostname ", "$time "}},
		// group by and order by.
		{line: q + " group by $hostname ", candidates: []string{"where ", "order by ", "limit "},
			hint: commaItemHint},
		{line: q + " where a == 1 group by $hostname order by ", want: q +
			" where a == 1 group by $hostname order by count(currentConnections) "},
		{line: q + " where a == 1 group by $hostname order by count(currentConnections) ", want: q +
			" where a == 1 group by $hostname order by count(currentConnections) limit "},
		{line: q + " order ", want: q + " order by "},
		{line: q + " rorder by count(currentConnections) ", candidates: []string{"where ", "group by ", "limit "}},
		// Nothing left to offer.
		{line: q + " where a == 1 group by a order by a limit 3 "},
	})
}

// TestGuidedQueryOperatorsOnly checks that the operator position offers
// operators only and the value position values only, never fields.
func TestGuidedQueryOperatorsOnly(t *testing.T) {
	line := ":query from STATS select count(currentConnections) where currentConnections "
	got := completeInteractive(line, len(line), guideArgs, catalogOf(guideCatalog))
	var operators []string
	for _, op := range mapr.WhereOperators() {
		operators = append(operators, op.Name+" ")
	}
	if !slices.Equal(got.candidates, operators) {
		t.Fatalf("operator position: %q, want %q", got.candidates, operators)
	}
	// A field without sample values: no value, and no field either.
	line = ":query from STATS select count(currentConnections) where lifetimeConnections eq "
	got = completeInteractive(line, len(line), guideArgs, catalogOf(guideCatalog))
	if got.line != line+`"1" ` {
		t.Fatalf("value position: line %q candidates %q", got.line, got.candidates)
	}
}

// TestGuidedQueryTableScope checks that fields, $variables and values come
// only from the query's table, also when it is typed after the cursor,
// and that the table probe is asked for that table.
func TestGuidedQueryTableScope(t *testing.T) {
	checkGuideSteps(t, guideArgs, guideCatalog, []guideStep{
		{line: ":query from ", candidates: []string{"OTHER ", "STATS "}},
		{line: ":query from OTHER select ", candidates: append(aggregationWords(), "bytes ", "$loglevel ")},
		{line: ":query from OTHER select count(", candidates: []string{"bytes)", "$loglevel)"}},
		{line: ":query select count(|) from OTHER", candidates: []string{"bytes)", "$loglevel)"}},
		{line: ":query from OTHER select bytes where $hostname eq "},
		{line: ":query from OTHER select bytes where $loglevel eq ",
			want: `:query from OTHER select bytes where $loglevel eq "INFO" `},
		// A later from clause replaces the earlier one, as in the parser.
		{line: ":query from STATS select | from OTHER", candidates: append(aggregationWords(),
			"bytes ", "$loglevel ")},
	})
	line := ":query select count(| from OTHER"
	pos := strings.IndexByte(line, '|')
	line = line[:pos] + line[pos+1:]
	if got := completeInteractive(line, pos, guideArgs, catalogOf(guideCatalog)); got.probeTable != "OTHER" {
		t.Fatalf("probe table %q, want the table after the cursor", got.probeTable)
	}
	// The table under the cursor is still being typed: no probe of it.
	if got := completeInteractive(":query from STA", len(":query from STA"), guideArgs,
		catalogOf(guideCatalog)); got.probeTable != "" {
		t.Fatalf("probe table %q for a table being typed", got.probeTable)
	}
}

// TestGuidedQueryHandTypedOrder checks a query begun with select: fields of
// every table until a table is known, a hint that a table narrows them,
// and from offered among the next clauses.
func TestGuidedQueryHandTypedOrder(t *testing.T) {
	line := ":query select "
	got := completeInteractive(line, len(line), guideArgs, catalogOf(guideCatalog))
	for _, want := range []string{"count(", "bytes ", "currentConnections ", "$hostname ", "$loglevel "} {
		if !slices.Contains(got.candidates, want) || got.hint != tableFirstHint {
			t.Fatalf("select first: candidates %q hint %q, want %q and the table hint",
				got.candidates, got.hint, want)
		}
	}
	checkGuideSteps(t, guideArgs, guideCatalog, []guideStep{
		{line: ":query sel", want: ":query select "},
		{line: ":query select count(bytes) ", candidates: []string{"from ", "where ", "group by ",
			"order by ", "limit "}, hint: commaItemHint},
		{line: ":query select count(bytes) from ", candidates: []string{"OTHER ", "STATS "}},
		{line: ":query select count(bytes) from OTHER ", candidates: []string{"where ", "group by ",
			"order by ", "limit "}},
		// select missing: it is the only next clause (from before it).
		{line: ":query from OTHER where bytes > 1 ", candidates: []string{"and ", "select "},
			hint: commaConditionHint},
		{line: ":query from OTHER limit 3 ", want: ":query from OTHER limit 3 select "},
		{line: ":query where bytes > 1 ", candidates: []string{"and ", "from "}, hint: commaConditionHint},
	})
}

// TestGuidedQueryWithoutTables checks a log format without tables: the
// builder starts at select and offers no from.
func TestGuidedQueryWithoutTables(t *testing.T) {
	catalog := catalogSnapshot{DefaultLogFormat: "csv", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"csv": {
			Tables: map[string]map[string][]string{"": {"name": {"x"}}},
		}}}
	checkGuideSteps(t, guideArgs, catalog, []guideStep{
		{line: ":query ", want: ":query select "},
		{line: ":query select name ", candidates: []string{"where ", "group by ", "order by ", "limit "},
			hint: commaItemHint},
	})
}

// TestGuidedQueryStartsWithFrom checks that a query begun with Tab starts
// with from: from and the table in one Tab when the catalog knows one
// table of the format, from and the tables listed when it knows several,
// and from with tableNameHint when it knows none (an empty catalog), in a
// :query line and in an empty --query value. A word typed first still
// completes to any clause.
func TestGuidedQueryStartsWithFrom(t *testing.T) {
	empty := catalogSnapshot{}
	for _, tt := range []struct {
		name    string
		args    config.Args
		catalog catalogSnapshot
		steps   []guideStep
	}{
		{name: "one table", args: guideArgs, catalog: statsOnlyCatalog, steps: []guideStep{
			{line: ":query ", want: ":query from STATS "},
			{line: ":query  ", want: ":query  from STATS "},
		}},
		{name: "several tables", args: guideArgs, catalog: guideCatalog, steps: []guideStep{
			{line: ":query ", want: ":query from ", candidates: []string{"OTHER ", "STATS "}},
			{line: ":query from ", candidates: []string{"OTHER ", "STATS "}},
		}},
		{name: "no table known", args: guideArgs, catalog: empty, steps: []guideStep{
			{line: ":query ", want: ":query from ", hint: joinLines(tableNameHint, queryCatalogHint)},
			{line: ":query from ", hint: joinLines(tableNameHint, queryCatalogHint)},
			{line: ":query from X ", want: ":query from X select "},
		}},
		{name: "--query value", args: queryArgs, catalog: statsOnlyCatalog, steps: []guideStep{
			{line: ":reload --query '", want: ":reload --query 'from STATS "},
			{line: ":reload --query '|'", want: ":reload --query 'from STATS '"},
			{line: `:reload --query "`, want: `:reload --query "from STATS `},
		}},
		{name: "--query value several tables", args: queryArgs, catalog: guideCatalog, steps: []guideStep{
			{line: ":reload --query '", want: ":reload --query 'from ", candidates: []string{"OTHER ", "STATS "}},
		}},
		// Plain logs: the servers answered, but only formats without tables
		// (generic) found fields, the default format none at all.
		{name: "answered without tables", args: guideArgs, catalog: genericOnlyCatalog, steps: []guideStep{
			{line: ":query ", want: ":query select "},
		}},
		// From as the only clause left later in a query: with its one table.
		{name: "from left last", args: guideArgs, catalog: statsOnlyCatalog, steps: []guideStep{
			{line: ":query select count(lifetimeConnections) where a == 1 group by $time limit 3 ",
				candidates: []string{"from ", "order by "}},
			{line: ":query select count(lifetimeConnections) where a == 1 group by $time order by $time limit 3 ",
				want: ":query select count(lifetimeConnections) where a == 1 group by $time order by $time limit 3 from STATS "},
		}},
		{name: "typed word", args: guideArgs, catalog: statsOnlyCatalog, steps: []guideStep{
			{line: ":query sel", want: ":query select "},
			{line: ":query f", want: ":query from "},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) { checkGuideSteps(t, tt.args, tt.catalog, tt.steps) })
	}
}

// TestGuidedQueryReloadValue checks the same guidance inside a quoted
// :reload --query value.
func TestGuidedQueryReloadValue(t *testing.T) {
	checkGuideSteps(t, queryArgs, statsOnlyCatalog, []guideStep{
		{line: ":reload --query 'from STATS ", want: ":reload --query 'from STATS select "},
		{line: ":reload --query 'from STATS select count(currentConnections) gro",
			want: ":reload --query 'from STATS select count(currentConnections) group by "},
		{line: ":reload --query 'from STATS select count(currentConnections) ",
			candidates: []string{"where ", "group by ", "order by ", "limit "}, hint: commaItemHint},
		{line: ":reload --query 'select count(|) from STATS'", candidates: []string{"currentConnections)",
			"lifetimeConnections)", "$hostname)", "$time)"}},
	})
}

// TestGuidedQueryCompletesToValidQueries follows the guided session Tab by
// Tab, typing only where Tab lists candidates, and checks that the query
// it builds parses.
func TestGuidedQueryCompletesToValidQueries(t *testing.T) {
	for _, typed := range [][]string{
		{"", "", "count(", "currentConnections", " ", "gro", "$hostname ", "", "", "10"},
		{"", "", "max(", "lifetimeConnections", " ", "w", "currentConnections", " ", ">", " ", "0", " ", "and ",
			"$hostname", " eq ", "", "o", "", " ", "l", "5"},
	} {
		line := ":query "
		for _, text := range typed {
			line += text
			line = completeInteractive(line, len(line), guideArgs, catalogOf(statsOnlyCatalog)).line
		}
		query := strings.TrimPrefix(line, ":query ")
		if _, err := mapr.NewQuery(query, nil); err != nil {
			t.Errorf("built %q, which does not parse: %v", query, err)
		}
	}
}

// TestGuidedClausesCoverKeywords checks that the guided and the rare
// clauses together are exactly the clause keywords of the query language.
func TestGuidedClausesCoverKeywords(t *testing.T) {
	var names []string
	for _, clause := range append(slices.Clone(guidedQueryClauses), rareQueryClauses...) {
		names = append(names, clause.keyword)
		if !strings.HasPrefix(clause.insert, clause.keyword+" ") {
			t.Errorf("clause %q inserts %q", clause.keyword, clause.insert)
		}
	}
	slices.Sort(names)
	want := mapr.Keywords()
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("clauses %q, keywords %q", names, want)
	}
}

// TestGuidedQueryLeadWordInTail checks that by and append are not inserted
// again when the text after the cursor already starts with them.
func TestGuidedQueryLeadWordInTail(t *testing.T) {
	const q = ":query from STATS select count(currentConnections)"
	checkGuideSteps(t, guideArgs, guideCatalog, []guideStep{
		{line: q + " gro| by $hostname", want: q + " group  by $hostname"},
		{line: q + " ror| BY count(currentConnections)", want: q + " rorder  BY count(currentConnections)"},
		{line: q + " order |by count(currentConnections)"},
		{line: q + ` outfile |append "/x"`},
		// Another word after the cursor: by is inserted.
		{line: q + " gro| limit 3", want: q + " group by  limit 3"},
	})
	for _, query := range []string{"from STATS select count(x) group  by x",
		"from STATS select count(x) rorder  BY count(x)"} {
		parsed, err := mapr.NewQuery(query, nil)
		if err != nil || len(parsed.GroupBy) > 1 || slices.Contains(parsed.GroupBy, "by") {
			t.Fatalf("%q: group by %q (%v)", query, parsed.GroupBy, err)
		}
	}
}

// TestGuidedQuerySkipsSelectedItems checks that an item already selected
// (the parser rejects duplicates) is not offered again.
func TestGuidedQuerySkipsSelectedItems(t *testing.T) {
	checkGuideSteps(t, guideArgs, statsOnlyCatalog, []guideStep{
		{line: ":query from STATS select currentConnections, ", candidates: append(aggregationWords(),
			"lifetimeConnections ", "$hostname ", "$time ")},
		{line: ":query from STATS select count(currentConnections), count(", candidates: []string{
			"lifetimeConnections)", "$hostname)", "$time)"}},
		// The same field in another aggregation is fine.
		{line: ":query from STATS select count(currentConnections), sum(c", want: ":query from STATS select " +
			"count(currentConnections), sum(currentConnections)"},
	})
}

// TestGuidedQueryHints checks when the table and catalog hints are shown:
// the table hint only with fields or $variables offered, the catalog hint
// (after the table name hint) also at the first step of an empty catalog.
func TestGuidedQueryHints(t *testing.T) {
	checkGuideSteps(t, guideArgs, guideCatalog, []guideStep{
		{line: ":query select count(bytes) f", want: ":query select count(bytes) from "},
		{line: ":query select count(bytes) gro", want: ":query select count(bytes) group by "},
		{line: ":query select count(xyz"},
		{line: ":query select count(by", want: ":query select count(bytes)", hint: tableFirstHint},
	})
	checkGuideSteps(t, guideArgs, catalogSnapshot{}, []guideStep{
		{line: ":query ", want: ":query from ", hint: joinLines(tableNameHint, queryCatalogHint)},
	})
}

// TestQuerySamplesOrdered checks that the sample values of a field found in
// several tables come in table order, the same on every call.
func TestQuerySamplesOrdered(t *testing.T) {
	tables := map[string]map[string][]string{}
	var want []string
	for _, name := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		tables[name] = map[string][]string{"n": {name + "1"}}
		want = append(want, `"`+name+`1" `)
	}
	catalog := catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {Tables: tables}}}
	for range 20 {
		got := completeQueryWord("select n where n eq ", "", catalog)
		if !slices.Equal(got.candidates, want) {
			t.Fatalf("values %q, want %q", got.candidates, want)
		}
	}
}

// TestGuidedQuerySetAndWhereComma covers a finished set assignment (the
// next clauses with a hint, no $variables mixed in) and a field named and,
// which can not be written as the left operand after a where condition.
func TestGuidedQuerySetAndWhereComma(t *testing.T) {
	const q = ":query from STATS select count(currentConnections)"
	checkGuideSteps(t, guideArgs, guideCatalog, []guideStep{
		{line: q + " set $x = 1 ", candidates: []string{"where ", "group by ", "order by ", "limit "},
			hint: setAssignmentHint},
		{line: q + " set $x = 1 $h", want: q + " set $x = 1 $hostname "},
	})
	catalog := catalogOf(catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {
			Tables:         map[string]map[string][]string{"T": {"and": {"1"}, "b": {"2"}}},
			TableVariables: map[string]map[string][]string{"T": {}},
		}}})
	for line, want := range map[string][]string{
		":query from T select b where b == 2, ": {"b "},
		":query from T select b where ":         {"and ", "b "},
	} {
		got := completeInteractive(line, len(line), guideArgs, catalog)
		if !slices.Equal(got.candidates, want) && got.line != line+want[0] {
			t.Errorf("%q: line %q candidates %q, want %q", line, got.line, got.candidates, want)
		}
	}
}
