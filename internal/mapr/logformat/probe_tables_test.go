package logformat

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestProbeTableVariables(t *testing.T) {
	sample := probeSampleOf("a.log", probeStatsLine, probeOrdersLine,
		"INFO|20211002-071209|1|other.go:7|8|13|7|0.21|1s|MAPREDUCE:STATS|currentConnections=4")
	result := mustProbe(t, "default", sample)

	if got := sortedKeys(result.TableVariables); !slices.Equal(got, []string{"ORDERS", "STATS"}) {
		t.Fatalf("table variables of tables %v, want [ORDERS STATS]", got)
	}
	if got := result.TableVariables["STATS"]["$caller"]; !slices.Equal(got, []string{"stats.go:56", "other.go:7"}) {
		t.Errorf("STATS $caller = %v, want only the callers of STATS lines", got)
	}
	if got := result.TableVariables["ORDERS"]["$caller"]; !slices.Equal(got, []string{"orders.go:10"}) {
		t.Errorf("ORDERS $caller = %v", got)
	}
	// $date is only known (11 character timestamp of the ORDERS line), so
	// the table of that line does not list it: per table only what its lines
	// filled.
	if _, ok := result.TableVariables["ORDERS"]["$date"]; ok {
		t.Error("ORDERS lists $date, which its lines did not fill")
	}
	// The format-wide view stays.
	if got := result.Variables["$caller"]; len(got) != 3 {
		t.Errorf("format-wide $caller = %v, want the callers of all lines", got)
	}
}

// TestProbeTableVariablesOfOneTable: a result of one table carries its
// per-table variables too, with only the variables its lines filled, never
// the format's known variables without values.
func TestProbeTableVariablesOfOneTable(t *testing.T) {
	// The 11 character timestamp of the ORDERS line fills no $date.
	result := mustProbe(t, "default", probeSampleOf("a.log", probeOrdersLine))
	orders := result.VariablesOf("ORDERS")
	if got := orders["$caller"]; !slices.Equal(got, []string{"orders.go:10"}) {
		t.Errorf("ORDERS $caller = %v", got)
	}
	if _, ok := orders["$date"]; ok {
		t.Errorf("ORDERS lists the known variable $date its lines did not fill: %v", orders)
	}
	if _, ok := result.Variables["$date"]; !ok {
		t.Error("the format-wide view lost the known variable $date")
	}
	result = mustProbe(t, "generickv", probeSampleOf("a.log", "a=1|b=2"))
	if got := result.VariablesOf(ProbeAnyTable)["$line"]; !slices.Equal(got, []string{"a=1|b=2"}) {
		t.Errorf("generickv $line = %v", got)
	}
	if _, ok := result.VariablesOf(ProbeAnyTable)["$timezone"]; !ok {
		t.Error("generickv lost a variable its lines filled")
	}
}

// TestProbeVariablesOfFallback: only a result without per-table data falls
// back to the format-wide variables; a table missing from per-table data
// has none.
func TestProbeVariablesOfFallback(t *testing.T) {
	without := ProbeResult{Tables: map[string]map[string][]string{"A": {}},
		Variables: map[string][]string{"$x": {"1"}}}
	if got := without.VariablesOf("A"); got["$x"] == nil {
		t.Errorf("without per-table data VariablesOf(A) = %v, want the format-wide ones", got)
	}
	with := without
	with.TableVariables = map[string]map[string][]string{"B": {"$y": {"2"}}}
	if got := with.VariablesOf("A"); got == nil || len(got) != 0 {
		t.Errorf("with per-table data VariablesOf(A) = %#v, want empty", got)
	}
	filtered, _ := without.FilterTable("A")
	if filtered.TableVariables["A"]["$x"] == nil {
		t.Errorf("FilterTable without per-table data = %+v, want the format-wide variables", filtered)
	}
}

func TestProbeCapsTableVariables(t *testing.T) {
	own := ProbeResult{
		Tables: map[string]map[string][]string{"A": {"f": nil}, "B": {"g": nil}},
		TableVariables: map[string]map[string][]string{
			"A":     {"$v": {"1", "2", "3", "4", "5", "6"}},
			"NOTAB": {"$w": {"x"}},
		},
	}
	for i := 0; i < ProbeMaxFieldsPerTable+5; i++ {
		own.TableVariables["A"][fmt.Sprintf("$x%03d", i)] = nil
	}
	registerParserFactoryForTest(t, "unit-test-table-vars", func(string, string, int) (Parser, error) {
		return &proberParser{result: own}, nil
	})
	result := mustProbe(t, "unit-test-table-vars", ProbeSample{})

	if _, ok := result.TableVariables["NOTAB"]; ok {
		t.Error("variables of a table the result does not list were kept")
	}
	if got := len(result.TableVariables["A"]); got != ProbeMaxFieldsPerTable {
		t.Errorf("A variables = %d, want the cap %d", got, ProbeMaxFieldsPerTable)
	}
	if got := result.TableVariables["A"]["$v"]; len(got) != ProbeMaxValuesPerField {
		t.Errorf("A $v = %v, want %d values", got, ProbeMaxValuesPerField)
	}
	// A Prober that gives per-table data but none for table B: B gets an
	// empty entry, so VariablesOf(B) offers nothing rather than the
	// format-wide variables.
	if got, ok := result.TableVariables["B"]; !ok || len(got) != 0 {
		t.Errorf("B table variables = %v, %v: want an empty entry", got, ok)
	}
}

// TestProbeProberWithoutTableVariables: a Prober that sets no per-table data
// keeps none, so VariablesOf falls back to the format-wide variables.
func TestProbeProberWithoutTableVariables(t *testing.T) {
	own := ProbeResult{Tables: map[string]map[string][]string{"A": {"f": nil}},
		Variables: map[string][]string{"$v": {"1"}}}
	registerParserFactoryForTest(t, "unit-test-no-table-vars", func(string, string, int) (Parser, error) {
		return &proberParser{result: own}, nil
	})
	result := mustProbe(t, "unit-test-no-table-vars", ProbeSample{})
	if result.TableVariables != nil {
		t.Fatalf("table variables = %v, want none", result.TableVariables)
	}
	if got := result.VariablesOf("A")["$v"]; !slices.Equal(got, []string{"1"}) {
		t.Errorf("VariablesOf(A) $v = %v, want the format-wide value", got)
	}
}

func TestProbeTableScanner(t *testing.T) {
	scanner := NewProbeTableScanner([]string{"default", "generic", "no-such-format"}, "h")
	for _, line := range []string{
		probeStatsLine, probeOrdersLine,
		"INFO|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:RARE|a=1",
		"INFO|1|1|x.go:1|8|13|7|0.21|1s|NOTMAPREDUCE|a=1",
		"WARN|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:WARNED|a=1", // the parser ignores non-INFO lines
		"INFO|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:SHORT",      // fewer than 11 tokens
		"a plain line",
	} {
		scanner.Scan([]byte(line))
	}
	if got := scanner.Tables("default"); !slices.Equal(got, []string{"ORDERS", "RARE", "STATS"}) {
		t.Errorf("default tables = %v, want [ORDERS RARE STATS]", got)
	}
	if got := scanner.Tables("generic"); got != nil {
		t.Errorf("generic tables = %v, want none (no tables)", got)
	}

	result := scanner.AddTables("default", mustProbe(t, "default", probeSampleOf("a", probeStatsLine)))
	if fields, ok := result.Tables["RARE"]; !ok || len(fields) != 0 {
		t.Errorf("RARE = %v, %v: want the scanned table without fields", fields, ok)
	}
	if got := result.Tables["STATS"]["currentConnections"]; !slices.Equal(got, []string{"0"}) {
		t.Errorf("STATS fields changed: %v", got)
	}
	if got, ok := result.TableVariables["RARE"]; !ok || len(got) != 0 {
		t.Errorf("RARE table variables = %v, %v: want an empty entry (no kept line filled one)", got, ok)
	}
	if got := result.VariablesOf("STATS")["$caller"]; !slices.Equal(got, []string{"stats.go:56"}) {
		t.Errorf("STATS $caller = %v", got)
	}
}

// panickingTabler is a test format whose ProbeLineTable panics.
type panickingTabler struct {
	fieldParser
	calls int
}

func (p *panickingTabler) ProbeLineTable(string) (string, bool) {
	p.calls++
	panic("tabler exploded")
}

// TestProbeTableScannerRecoversPanics: a panicking ProbeTabler neither
// aborts the scan nor the matcher, is not called again, and is reported.
func TestProbeTableScannerRecoversPanics(t *testing.T) {
	tabler := &panickingTabler{}
	registerParserFactoryForTest(t, "unit-test-panicking-tabler", func(string, string, int) (Parser, error) {
		return tabler, nil
	})
	scanner := NewProbeTableScanner([]string{"default", "unit-test-panicking-tabler"}, "h")
	scanner.Scan([]byte(probeStatsLine))
	scanner.Scan([]byte(probeOrdersLine))
	if !scanner.Matcher("ORDERS")([]byte(probeOrdersLine)) {
		t.Error("the matcher lost the default format's ORDERS line")
	}
	if got := scanner.Tables("default"); !slices.Equal(got, []string{"ORDERS", "STATS"}) {
		t.Errorf("default tables = %v", got)
	}
	failed := scanner.Failed()
	if err := failed["unit-test-panicking-tabler"]; err == nil || !strings.Contains(err.Error(), "tabler exploded") {
		t.Fatalf("failed = %v, want the panicking format", failed)
	}
	if len(failed) != 1 || tabler.calls != 1 {
		t.Errorf("failed = %v after %d calls, want one format called once", failed, tabler.calls)
	}
}

// TestProbeTableScannerMatcher: the filter of a table-targeted probe keeps
// the lines a ProbeTabler assigns to the table, drops lines it assigns to
// another table even when they name the table, and falls back to the word
// match for lines no ProbeTabler claims.
func TestProbeTableScannerMatcher(t *testing.T) {
	match := NewProbeTableScanner([]string{"default", "generic"}, "h").Matcher("STATS")
	tests := []struct {
		line string
		want bool
	}{
		{probeStatsLine, true},
		{probeOrdersLine, false},
		{"INFO|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:ORDERS|note=STATS", false},
		{"INFO|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:STATSX|a=1", false},
		{"a custom STATS line", true},
		{"a custom STATSX line", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := match([]byte(tt.line)); got != tt.want {
			t.Errorf("match(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
	if !NewProbeTableScanner(nil, "h").Matcher("")([]byte("x")) {
		t.Error("the matcher of no table must keep every line")
	}
}

func TestProbeTableScannerCapsTables(t *testing.T) {
	scanner := NewProbeTableScanner([]string{"default"}, "h")
	for i := 0; i < ProbeMaxTables+10; i++ {
		scanner.Scan(fmt.Appendf(nil, "INFO|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:T%03d|a=1", i))
	}
	if got := len(scanner.Tables("default")); got != ProbeMaxTables {
		t.Errorf("tables = %d, want the cap %d", got, ProbeMaxTables)
	}
}

func TestProbeFilterTable(t *testing.T) {
	sample := probeSampleOf("a.log", probeStatsLine, probeOrdersLine)
	result := mustProbe(t, "default", sample)

	stats, ok := result.FilterTable("STATS")
	if !ok {
		t.Fatal("FilterTable(STATS) found nothing")
	}
	if got := sortedKeys(stats.Tables); !slices.Equal(got, []string{"STATS"}) {
		t.Errorf("tables = %v, want [STATS]", got)
	}
	if got := stats.TableVariables["STATS"]["$caller"]; !slices.Equal(got, []string{"stats.go:56"}) {
		t.Errorf("STATS $caller = %v", got)
	}
	if stats.Variables != nil {
		t.Errorf("format-wide variables = %v, want none in a table-targeted result", stats.Variables)
	}
	if _, found := result.FilterTable("stats"); found {
		t.Error("FilterTable matched a table of another case")
	}
	if _, found := result.FilterTable("MISSING"); found {
		t.Error("FilterTable found a table the result does not list")
	}

	// One table only: its variables are the format-wide ones.
	single := mustProbe(t, "default", probeSampleOf("a.log", probeStatsLine))
	filtered, ok := single.FilterTable("STATS")
	if !ok || filtered.TableVariables["STATS"]["$caller"] == nil {
		t.Errorf("single table filtered = %+v, %v", filtered, ok)
	}
}

func TestProbeLineTableMatcher(t *testing.T) {
	match := ProbeTableLineMatcher("STATS")
	tests := []struct {
		line string
		want bool
	}{
		{probeStatsLine, true},
		{probeOrdersLine, false},
		{"x|MAPREDUCE:STATSX|a=1", false},
		{"x|MAPREDUCE:XSTATS|a=1", false},
		{"x|MAPREDUCE:STATS_2|a=1", false},
		{"STATS", true},
		{"x STATS y", true},
		{"x|MAPREDUCE:STATS", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := match([]byte(tt.line)); got != tt.want {
			t.Errorf("match(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
	if ProbeTableLineMatcher("")([]byte("anything")) != true {
		t.Error("the matcher of no table must keep every line")
	}
}

// TestProbeTableScannerMatcherCutName: the scanner lists a table name cut
// to ProbeMaxNameBytes, which is the name a table-targeted probe then asks
// for, so the matcher must compare the cut names too. A table whose name
// differs within the first ProbeMaxNameBytes bytes is still dropped.
func TestProbeTableScannerMatcherCutName(t *testing.T) {
	const prefix = "INFO|1|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:"
	long := strings.Repeat("T", ProbeMaxNameBytes) + "TAIL"
	line := []byte(prefix + long + "|a=1")
	scanner := NewProbeTableScanner([]string{"default"}, "h")
	scanner.Scan(line)
	listed := scanner.Tables("default")
	if len(listed) != 1 || len(listed[0]) != ProbeMaxNameBytes {
		t.Fatalf("tables = %v, want one name of %d bytes", listed, ProbeMaxNameBytes)
	}
	match := scanner.Matcher(listed[0])
	if !match(line) {
		t.Errorf("the matcher of the listed name %q drops the table's line", listed[0])
	}
	other := strings.Repeat("T", ProbeMaxNameBytes-1) + "XTAIL"
	if match([]byte(prefix + other + "|a=1")) {
		t.Error("the matcher keeps a table that differs before the cut")
	}
	if match([]byte(prefix + "ORDERS|note=" + listed[0])) {
		t.Error("the matcher keeps a line of another table that names the table")
	}
}

// TestProbeCollectorAddFieldsCapIsDeterministic: of a line with more fields
// (and $variables) than a table keeps, the first ones in name order
// survive, not a random set of the map's iteration order.
func TestProbeCollectorAddFieldsCapIsDeterministic(t *testing.T) {
	fields := make(map[string]string)
	var wantFields, wantVariables []string
	for i := 0; i < ProbeMaxFieldsPerTable+50; i++ {
		field, variable := fmt.Sprintf("f%04d", i), fmt.Sprintf("$v%04d", i)
		fields[field], fields[variable] = "1", "2"
		if i < ProbeMaxFieldsPerTable {
			wantFields, wantVariables = append(wantFields, field), append(wantVariables, variable)
		}
	}
	fields["*"] = "x"
	var collector probeCollector
	collector.addFields("A", fields)
	result := collector.result
	if got := sortedKeys(result.Tables["A"]); !slices.Equal(got, wantFields) {
		t.Errorf("fields kept differ from the first %d by name: %d kept", ProbeMaxFieldsPerTable, len(got))
	}
	if got := sortedKeys(result.Variables); !slices.Equal(got, wantVariables) {
		t.Errorf("variables kept differ from the first %d by name: %d kept", ProbeMaxFieldsPerTable, len(got))
	}
	if got := sortedKeys(result.TableVariables["A"]); !slices.Equal(got, wantVariables) {
		t.Errorf("table variables kept differ from the first %d by name: %d kept", ProbeMaxFieldsPerTable, len(got))
	}
}

// TestProbeTableLineMatcherCutName: the word match, which serves the
// formats without a ProbeTabler, keeps the lines of a table whose name is
// longer than ProbeMaxNameBytes when asked for the name the probe lists
// (cut), and only those: a shorter name still needs the whole word, and a
// name that differs before the cut is another table.
func TestProbeTableLineMatcherCutName(t *testing.T) {
	listed := strings.Repeat("T", ProbeMaxNameBytes)
	short := strings.Repeat("T", ProbeMaxNameBytes-1)
	// The two bytes of the umlaut straddle the cut, so the listed name
	// ends before it.
	umlaut := short + "\u00c4TAIL"
	tests := []struct {
		table, line string
		want        bool
	}{
		{listed, "table=" + listed + "TAIL a=1", true},
		{listed, "table=" + listed + "_2 a=1", true},
		{listed, "table=" + listed + " a=1", true},
		{listed, listed, true},
		{listed, "table=X" + listed + "TAIL a=1", false},
		{listed, "table=" + short + "XTAIL a=1", false},
		{short, "table=" + listed + " a=1", false},
		{short, "table=" + short + "X a=1", false},
		{short, "table=" + umlaut + " a=1", true},
		{truncateUTF8(umlaut, ProbeMaxNameBytes), "table=" + umlaut + " a=1", true},
		{"STATS", "table=STATSX and " + strings.Repeat("x", 2*ProbeMaxNameBytes), false},
	}
	for _, tt := range tests {
		for name, match := range map[string]func([]byte) bool{
			"ProbeTableLineMatcher":    ProbeTableLineMatcher(tt.table),
			"nil scanner":              (*ProbeTableScanner)(nil).Matcher(tt.table),
			"scanner, unclaimed line":  NewProbeTableScanner([]string{"default"}, "h").Matcher(tt.table),
			"scanner without a tabler": NewProbeTableScanner([]string{"generic"}, "h").Matcher(tt.table),
		} {
			if got := match([]byte(tt.line)); got != tt.want {
				t.Errorf("%s(%d bytes)(%q) = %v, want %v", name, len(tt.table), tt.line, got, tt.want)
			}
		}
	}
}
