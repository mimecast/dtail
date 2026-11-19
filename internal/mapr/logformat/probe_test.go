package logformat

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	probeStatsLine = "INFO|20211002-071209|1|stats.go:56|8|13|7|0.21|471h0m21s|" +
		"MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"
	probeOrdersLine = "INFO|1002-071143|1|orders.go:10|8|13|7|0.21|471h0m21s|" +
		"MAPREDUCE:ORDERS|orderID=42|amount=7"
)

func probeSampleOf(sourceID string, lines ...string) ProbeSample {
	var sample ProbeSample
	for _, line := range lines {
		sample.Lines = append(sample.Lines, ProbeLine{Line: line, SourceID: sourceID})
	}
	return sample
}

func mustProbe(t *testing.T, name string, sample ProbeSample) ProbeResult {
	t.Helper()
	result, err := Probe(name, "probe-host", sample)
	if err != nil {
		t.Fatalf("Probe(%q): %v", name, err)
	}
	return result
}

func TestNamesListsBuiltInsSorted(t *testing.T) {
	names := Names()
	if !slices.IsSorted(names) {
		t.Fatalf("Names() not sorted: %v", names)
	}
	for _, want := range []string{"csv", "custom1", "custom2", "default", "generic",
		"generickv", "mimecast", "mimecastgeneric"} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() = %v, missing %q", names, want)
		}
	}
	registerParserFactoryForTest(t, "unit-test-names", func(string, string, int) (Parser, error) {
		return &testParser{}, nil
	})
	if !slices.Contains(Names(), "unit-test-names") {
		t.Errorf("Names() misses a format registered with RegisterParser")
	}
}

func TestKnownVariables(t *testing.T) {
	tests := []struct {
		format         string
		wantEnumerable bool
		want           []string
		unwant         []string
	}{
		{"default", true, []string{"$line", "$hostname", "$time", "$date", "$uptime"}, nil},
		{"generic", true, []string{"$line", "$timezone"}, []string{"$time", "$date"}},
		{"generickv", true, []string{"$empty"}, []string{"$pid"}},
		{"csv", true, []string{"$server"}, []string{"$caller"}},
		{"custom1", false, nil, nil},
		{"mimecast", false, nil, nil},
		{"no-such-format", false, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			got, enumerable := KnownVariables(tt.format)
			if enumerable != tt.wantEnumerable {
				t.Fatalf("enumerable = %v, want %v", enumerable, tt.wantEnumerable)
			}
			if !enumerable && got != nil {
				t.Fatalf("got %v for a non-enumerable format", got)
			}
			if !slices.IsSorted(got) {
				t.Errorf("not sorted: %v", got)
			}
			for _, name := range tt.want {
				if !slices.Contains(got, name) {
					t.Errorf("missing %s in %v", name, got)
				}
			}
			for _, name := range tt.unwant {
				if slices.Contains(got, name) {
					t.Errorf("unexpected %s in %v", name, got)
				}
			}
		})
	}
}

func TestKnownVariablesReturnsCopy(t *testing.T) {
	got, _ := KnownVariables("generic")
	got[0] = "$mutated"
	again, _ := KnownVariables("generic")
	if slices.Contains(again, "$mutated") {
		t.Fatal("KnownVariables shares its slice with the variable sets")
	}
	if _, ok := commonVariables["$mutated"]; ok {
		t.Fatal("KnownVariables let a caller change commonVariables")
	}
}

func TestProbeDefaultGroupsByTable(t *testing.T) {
	sample := probeSampleOf("a.log",
		probeStatsLine,
		"INFO|20211002-071209|1|stats.go:56|8|13|7|0.21|471h0m21s|"+
			"MAPREDUCE:STATS|currentConnections=3|lifetimeConnections=1",
		probeOrdersLine,
		"WARN|20211002-071209|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:IGNORED|a=b",
		"INFO|20211002-071209|1|x.go:1|8|13|7|0.21|1s|NOTMAPREDUCE|a=b",
		"INFO|20211002-071209|1|x.go:1|8|13|7|0.21|1s|MAPREDUCE:BROKEN|novalue",
		"a plain line",
	)
	result := mustProbe(t, "default", sample)

	if got := sortedKeys(result.Tables); !slices.Equal(got, []string{"ORDERS", "STATS"}) {
		t.Fatalf("tables = %v, want [ORDERS STATS]", got)
	}
	stats := result.Tables["STATS"]
	if got := stats["currentConnections"]; !slices.Equal(got, []string{"0", "3"}) {
		t.Errorf("STATS currentConnections = %v", got)
	}
	if got := stats["lifetimeConnections"]; !slices.Equal(got, []string{"1"}) {
		t.Errorf("STATS lifetimeConnections = %v (values must be distinct)", got)
	}
	if _, ok := stats["orderID"]; ok {
		t.Error("ORDERS field leaked into STATS")
	}
	if got := result.Tables["ORDERS"]["orderID"]; !slices.Equal(got, []string{"42"}) {
		t.Errorf("ORDERS orderID = %v", got)
	}
	if got := result.Variables["$hostname"]; !slices.Equal(got, []string{"probe-host"}) {
		t.Errorf("$hostname = %v, want [probe-host]", got)
	}
	if got := result.Variables["$date"]; !slices.Equal(got, []string{"20211002"}) {
		t.Errorf("$date = %v", got)
	}
	if _, ok := result.Variables["*"]; ok {
		t.Error("placeholder * listed as a variable")
	}
	if _, ok := stats["*"]; ok {
		t.Error("placeholder * listed as a field")
	}
}

func TestProbeAddsKnownVariablesWithoutValues(t *testing.T) {
	// The 11 character timestamp carries no $date, so it can only come from
	// KnownVariables.
	result := mustProbe(t, "default", probeSampleOf("a.log", probeOrdersLine))
	got, ok := result.Variables["$date"]
	if !ok {
		t.Fatal("known variable $date missing")
	}
	if got == nil || len(got) != 0 {
		t.Errorf("$date = %#v, want an empty, non-nil slice", got)
	}
}

func TestProbeBuiltInFormats(t *testing.T) {
	kvLine := "service=web|status=200|bytes=512"
	tests := []struct {
		name       string
		format     string
		sample     ProbeSample
		wantFields map[string][]string // fields of ProbeAnyTable
		wantVars   []string
		wantEmpty  bool
	}{
		{name: "generic has variables only", format: "generic",
			sample:   probeSampleOf("a", "any line at all"),
			wantVars: []string{"$line", "$hostname", "$timezone"}},
		{name: "generickv", format: "generickv",
			sample: probeSampleOf("a", kvLine, "service=db|garbage"),
			wantFields: map[string][]string{"service": {"web", "db"}, "status": {"200"},
				"bytes": {"512"}},
			wantVars: []string{"$line"}},
		{name: "csv uses the header of every source", format: "csv",
			sample: ProbeSample{Lines: []ProbeLine{
				{Line: "service,status", SourceID: "a"},
				{Line: "web,200", SourceID: "a"},
				{Line: "user,\"quoted, col\"", SourceID: "b"},
			}},
			wantFields: map[string][]string{"service": {"web"}, "status": {"200"},
				"user": {}, "quoted, col": {}},
			wantVars: []string{"$line"}},
		{name: "default parses no generic line", format: "default",
			sample: probeSampleOf("a", kvLine), wantEmpty: true},
		{name: "custom1 template", format: "custom1",
			sample: probeSampleOf("a", kvLine), wantEmpty: true},
		{name: "custom2 template", format: "custom2",
			sample: probeSampleOf("a", kvLine), wantEmpty: true},
		{name: "unknown format", format: "no-such-format",
			sample: probeSampleOf("a", kvLine), wantEmpty: true},
		{name: "empty sample", format: "generickv", wantEmpty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mustProbe(t, tt.format, tt.sample)
			if tt.wantEmpty {
				if !result.Empty() {
					t.Fatalf("result = %+v, want empty", result)
				}
				return
			}
			assertProbeFields(t, result, tt.wantFields)
			for _, name := range tt.wantVars {
				if _, ok := result.Variables[name]; !ok {
					t.Errorf("variable %s missing in %v", name, result.Variables)
				}
			}
		})
	}
}

func assertProbeFields(t *testing.T, result ProbeResult, want map[string][]string) {
	t.Helper()
	got := result.Tables[ProbeAnyTable]
	if len(got) != len(want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	for field, values := range want {
		if !slices.Equal(got[field], values) {
			t.Errorf("field %q = %v, want %v", field, got[field], values)
		}
	}
}

func TestProbeEveryRegisteredFormat(t *testing.T) {
	sample := probeSampleOf("a", probeStatsLine, "service=web|status=200", "a,b")
	for _, name := range Names() {
		if _, err := Probe(name, "probe-host", sample); err != nil {
			t.Errorf("Probe(%q): %v", name, err)
		}
	}
}

// fieldParser is a test format without a Prober: it reports the line as the
// bareword "word" and the variable "$len", and fails on lines starting with
// "!" and ignores lines starting with "#".
type fieldParser struct {
	released []string
}

func (p *fieldParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	switch {
	case strings.HasPrefix(maprLine, "!"):
		return nil, errors.New("broken line")
	case strings.HasPrefix(maprLine, "#"):
		return nil, ErrIgnoreFields
	}
	return map[string]string{"word": maprLine, "$len": fmt.Sprint(len(maprLine))}, nil
}

func (p *fieldParser) ReleaseSource(sourceID string) {
	p.released = append(p.released, sourceID)
}

// proberParser is a test format with a Prober of its own.
type proberParser struct {
	fieldParser
	result ProbeResult
	err    error
	panics bool
}

func (p *proberParser) Probe(sample ProbeSample) (ProbeResult, error) {
	if p.panics {
		panic("probe exploded")
	}
	return p.result, p.err
}

func TestProbeRegisteredFormatWithoutProber(t *testing.T) {
	parser := &fieldParser{}
	registerParserFactoryForTest(t, "unit-test-generic-probe", func(string, string, int) (Parser, error) {
		return parser, nil
	})
	sample := ProbeSample{Lines: []ProbeLine{
		{Line: "alpha", SourceID: "a"}, {Line: "# comment", SourceID: "a"},
		{Line: "!bad", SourceID: "b"}, {Line: "beta", SourceID: "b"},
	}}
	result := mustProbe(t, "unit-test-generic-probe", sample)

	assertProbeFields(t, result, map[string][]string{"word": {"alpha", "beta"}})
	if got := result.Variables["$len"]; !slices.Equal(got, []string{"5", "4"}) {
		t.Errorf("$len = %v", got)
	}
	if !slices.Equal(parser.released, []string{"a", "b"}) {
		t.Errorf("released sources = %v, want [a b]", parser.released)
	}
	if result.Note != "" {
		t.Errorf("generic probe set a note: %q", result.Note)
	}
}

func TestProbeRegisteredFormatWithProber(t *testing.T) {
	own := ProbeResult{
		Tables: map[string]map[string][]string{"FIXED": {"schemaField": nil}},
		Note:   "fixed schema",
	}
	register := func(t *testing.T, parser *proberParser) {
		registerParserFactoryForTest(t, "unit-test-own-probe", func(string, string, int) (Parser, error) {
			return parser, nil
		})
	}

	t.Run("own result is used", func(t *testing.T) {
		register(t, &proberParser{result: own})
		result := mustProbe(t, "unit-test-own-probe", probeSampleOf("a", "alpha"))
		if _, ok := result.Tables[ProbeAnyTable]; ok {
			t.Errorf("generic probe ran instead of the Prober: %v", result.Tables)
		}
		if got, ok := result.Tables["FIXED"]["schemaField"]; !ok || got == nil {
			t.Errorf("FIXED schemaField = %#v, %v", got, ok)
		}
		if result.Note != "fixed schema" {
			t.Errorf("note = %q", result.Note)
		}
	})
	t.Run("error is returned", func(t *testing.T) {
		wantErr := errors.New("probe failed")
		register(t, &proberParser{err: wantErr})
		_, err := Probe("unit-test-own-probe", "h", probeSampleOf("a", "alpha"))
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "unit-test-own-probe") {
			t.Fatalf("err = %v, want wrapped %v naming the format", err, wantErr)
		}
	})
	t.Run("panic becomes an error", func(t *testing.T) {
		register(t, &proberParser{panics: true})
		result, err := Probe("unit-test-own-probe", "h", probeSampleOf("a", "alpha"))
		if err == nil || !strings.Contains(err.Error(), "probe exploded") {
			t.Fatalf("err = %v, want the panic", err)
		}
		if !result.Empty() {
			t.Errorf("result = %+v, want empty", result)
		}
	})
}

func TestProbeWithParserNilParser(t *testing.T) {
	var parser *fieldParser
	if _, err := ProbeWithParser(parser, probeSampleOf("a", "x")); err == nil {
		t.Fatal("ProbeWithParser accepted a nil parser")
	}
	if _, err := ProbeWithParser(nil, probeSampleOf("a", "x")); err == nil {
		t.Fatal("ProbeWithParser accepted a nil interface")
	}
}

func TestProbeCapsGenericProbe(t *testing.T) {
	var lines []string
	for i := 0; i < ProbeMaxFieldsPerTable+10; i++ {
		lines = append(lines, fmt.Sprintf("field%03d=v%d", i, i))
	}
	for i := 0; i < ProbeMaxValuesPerField+3; i++ {
		lines = append(lines, fmt.Sprintf("field000=value%d", i))
	}
	long := strings.Repeat("a", ProbeMaxValueBytes-1) + "€tail"
	lines = append(lines, "field001="+long)
	result := mustProbe(t, "generickv", probeSampleOf("a", lines...))

	fields := result.Tables[ProbeAnyTable]
	if len(fields) != ProbeMaxFieldsPerTable {
		t.Errorf("fields = %d, want %d", len(fields), ProbeMaxFieldsPerTable)
	}
	if got := fields["field000"]; len(got) != ProbeMaxValuesPerField {
		t.Errorf("field000 values = %v, want %d", got, ProbeMaxValuesPerField)
	}
	assertProbeValuesCapped(t, result)
	values := fields["field001"]
	if len(values) != 2 || values[1] != strings.Repeat("a", ProbeMaxValueBytes-1) {
		t.Errorf("field001 = %q, want the long value cut before the euro sign", values)
	}
}

func TestProbeCapsProberResult(t *testing.T) {
	big := ProbeResult{Tables: map[string]map[string][]string{}, Variables: map[string][]string{},
		Note: strings.Repeat("n", ProbeMaxNoteBytes+10)}
	for i := 0; i < ProbeMaxTables+5; i++ {
		big.Tables[fmt.Sprintf("T%03d", i)] = map[string][]string{"f": {"x"}}
	}
	fields := big.Tables["T000"]
	for i := 0; i < ProbeMaxFieldsPerTable+5; i++ {
		fields[fmt.Sprintf("f%03d", i)] = []string{"1", "2", "3", "4", "5", "6", "7",
			strings.Repeat("x", 3*ProbeMaxValueBytes)}
	}
	for i := 0; i < ProbeMaxFieldsPerTable+5; i++ {
		big.Variables[fmt.Sprintf("$v%03d", i)] = nil
	}
	registerParserFactoryForTest(t, "unit-test-big-probe", func(string, string, int) (Parser, error) {
		return &proberParser{result: big}, nil
	})
	result := mustProbe(t, "unit-test-big-probe", ProbeSample{})

	if len(result.Tables) != ProbeMaxTables {
		t.Errorf("tables = %d, want %d", len(result.Tables), ProbeMaxTables)
	}
	if _, ok := result.Tables["T000"]; !ok {
		t.Error("caps dropped the first table in sorted order")
	}
	if len(result.Tables["T000"]) != ProbeMaxFieldsPerTable {
		t.Errorf("T000 fields = %d, want %d", len(result.Tables["T000"]), ProbeMaxFieldsPerTable)
	}
	if len(result.Variables) != ProbeMaxFieldsPerTable {
		t.Errorf("variables = %d, want %d", len(result.Variables), ProbeMaxFieldsPerTable)
	}
	if len(result.Note) != ProbeMaxNoteBytes {
		t.Errorf("note length = %d, want %d", len(result.Note), ProbeMaxNoteBytes)
	}
	assertProbeValuesCapped(t, result)
}

func assertProbeValuesCapped(t *testing.T, result ProbeResult) {
	t.Helper()
	check := func(where string, values []string) {
		if len(values) > ProbeMaxValuesPerField {
			t.Errorf("%s: %d values", where, len(values))
		}
		for _, value := range values {
			if len(value) > ProbeMaxValueBytes || !utf8.ValidString(value) {
				t.Errorf("%s: value %q not cut to %d bytes of UTF-8", where, value, ProbeMaxValueBytes)
			}
		}
	}
	for table, fields := range result.Tables {
		for field, values := range fields {
			check(table+"."+field, values)
		}
	}
	for name, values := range result.Variables {
		check(name, values)
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 3, "abc"},
		{"ab€", 4, "ab"},
		{"€€", 3, "€"},
		{"€", 0, ""},
	}
	for _, tt := range tests {
		if got := truncateUTF8(tt.in, tt.max); got != tt.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestProbeCapsNames(t *testing.T) {
	longKey := strings.Repeat("k", ProbeMaxNameBytes-1) + "€rest"
	cutKey := strings.Repeat("k", ProbeMaxNameBytes-1)

	t.Run("generic probe field names", func(t *testing.T) {
		result := mustProbe(t, "generickv", probeSampleOf("a", longKey+"=1", longKey+"x=2"))
		got, ok := result.Tables[ProbeAnyTable][cutKey]
		if !ok || len(result.Tables[ProbeAnyTable]) != 1 {
			t.Fatalf("fields = %v, want only %q", result.Tables[ProbeAnyTable], cutKey)
		}
		if !slices.Equal(got, []string{"1", "2"}) {
			t.Errorf("values = %v, want names equal after the cut merged", got)
		}
	})
	t.Run("csv probe of a line that is no csv", func(t *testing.T) {
		// No field named after the whole line, not even a cut one: the csv
		// probe takes no line without a plausible header for csv at all.
		result := mustProbe(t, "csv", probeSampleOf("a", strings.Repeat("x", 5000)))
		if !result.Empty() {
			t.Fatalf("result = %+v, want empty", result)
		}
	})
	t.Run("prober result names", func(t *testing.T) {
		big := ProbeResult{
			Tables:    map[string]map[string][]string{longKey: {longKey: {"v"}}},
			Variables: map[string][]string{"$" + longKey: {"v"}},
		}
		registerParserFactoryForTest(t, "unit-test-long-names", func(string, string, int) (Parser, error) {
			return &proberParser{result: big}, nil
		})
		result := mustProbe(t, "unit-test-long-names", ProbeSample{})
		if _, ok := result.Tables[cutKey][cutKey]; !ok {
			t.Errorf("tables = %v, want table and field cut to %q", result.Tables, cutKey)
		}
		if len(result.Variables) != 1 {
			t.Fatalf("variables = %v, want one", result.Variables)
		}
		for name := range result.Variables {
			if len(name) > ProbeMaxNameBytes || !utf8.ValidString(name) {
				t.Errorf("variable name %q not cut", name)
			}
		}
	})
}
