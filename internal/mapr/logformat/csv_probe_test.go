package logformat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCSVProbeIgnoresLinesThatAreNoCSV checks that the csv probe names fields
// only for a source whose first line is a plausible csv header: lines of
// another format (such as dserver's own `|`-separated log, with or without a
// comma in a message) must not come back as csv field names, while a real
// csv file, also one with a header only, keeps its columns.
func TestCSVProbeIgnoresLinesThatAreNoCSV(t *testing.T) {
	dserverLine := "INFO|1119-000001|134919|stats.go:104|4|16|0|0.00|47h42m19s|" +
		"MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"
	commaLine := "INFO|1119-000002|134920|probe.go:10|4|16|0|0.00|47h42m20s|" +
		"Probe answered|entries=1, errors=0"
	for name, sample := range map[string]ProbeSample{
		"pipe separated log":              probeSampleOf("a", dserverLine, dserverLine, commaLine),
		"comma in the first line":         probeSampleOf("a", commaLine, dserverLine, dserverLine),
		"one column":                      probeSampleOf("a", "name", "alpha", "beta"),
		"one column left of an empty one": probeSampleOf("a", "name,", "a,"),
		"column longer than a name":       probeSampleOf("a", strings.Repeat("x", ProbeMaxNameBytes+1)+",y", "1,2"),
		"rows of another width":           probeSampleOf("a", "a,b", "1", "2", "3,4,5"),
		"header that does not parse":      probeSampleOf("a", `a,"b`, "1,2"),
		"default probe stats line":        probeSampleOf("a", probeStatsLine, probeStatsLine),
		"no csv in either of two sources": {Lines: []ProbeLine{
			{Line: dserverLine, SourceID: "a"}, {Line: commaLine, SourceID: "b"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if result := mustProbe(t, "csv", sample); !result.Empty() {
				t.Fatalf("result = %+v, want empty", result)
			}
		})
	}

	t.Run("csv source next to one that is no csv", func(t *testing.T) {
		sample := ProbeSample{Lines: []ProbeLine{
			{Line: dserverLine, SourceID: "log"},
			{Line: dserverLine, SourceID: "log"},
			{Line: "service,status", SourceID: "csv"},
			{Line: "web,200", SourceID: "csv"},
		}}
		result := mustProbe(t, "csv", sample)
		assertProbeFields(t, result, map[string][]string{"service": {"web"}, "status": {"200"}})
	})
	t.Run("csv header only", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "service,status"))
		assertProbeFields(t, result, map[string][]string{"service": {}, "status": {}})
	})
	t.Run("empty columns are left out", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "host,status,", "web,200,", "db,500,"))
		assertProbeFields(t, result, map[string][]string{"host": {"web", "db"}, "status": {"200", "500"}})
		result = mustProbe(t, "csv", probeSampleOf("a", "name,,value", "a,b,c"))
		assertProbeFields(t, result, map[string][]string{"name": {"a"}, "value": {"c"}})
	})
	t.Run("rows with fewer columns than the header", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "a,b,c", "1,2", "3,4", "5,6,7"))
		assertProbeFields(t, result, map[string][]string{"a": {"1", "3", "5"}, "b": {"2", "4", "6"},
			"c": {"7"}})
	})
	t.Run("an over-long column is left out", func(t *testing.T) {
		long := strings.Repeat("x", ProbeMaxNameBytes+1)
		result := mustProbe(t, "csv", probeSampleOf("a", long+",b,c", "1,2,3"))
		assertProbeFields(t, result, map[string][]string{"b": {"2"}, "c": {"3"}})
	})
	t.Run("csv with a few odd rows", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "a,b", "1,2", "3,4", "5"))
		assertProbeFields(t, result, map[string][]string{"a": {"1", "3", "5"}, "b": {"2", "4"}})
	})
}

// TestCSVProbeRejectsFreeTextWithOneComma: a free-text log whose lines all
// hold one comma has the column count of a csv file, but its first line is
// a line like the others, not a header: its "column names" come back in the
// data rows at the same position (digits aside). Such a source is no csv.
func TestCSVProbeRejectsFreeTextWithOneComma(t *testing.T) {
	for name, sample := range map[string]ProbeSample{
		"same text after the comma": probeSampleOf("a",
			"request 1 done, ok", "request 2 done, ok", "request 3 done, ok", "request 4 done, ok"),
		"numbers on both sides": probeSampleOf("a",
			"request 1 done, took 5ms", "request 22 done, took 17ms", "request 333 done, took 2ms",
			"request 4444 done, took 9ms"),
		"syslog like": probeSampleOf("a",
			"Nov 19 12:00:01 host app[1]: user bob logged in, session 7",
			"Nov 19 12:00:02 host app[1]: user alice logged in, session 8",
			"Nov 19 12:00:09 host app[1]: user carol logged out, session 9",
			"Nov 19 12:00:11 host app[1]: user dave logged in, session 10"),
		"csv without a header row": probeSampleOf("a", "web,ok", "db,ok", "cache,ok", "lb,ok"),
	} {
		t.Run(name, func(t *testing.T) {
			if result := mustProbe(t, "csv", sample); !result.Empty() {
				t.Fatalf("result = %+v, want empty", result)
			}
		})
	}
}

// TestCSVProbeKeepsCSVWithHeaderLikeValues: real csv files are kept, also
// when some values look like their column name: a header repeated in a few
// rows (concatenated files), numeric column names (compared by no shape, as
// every number has the shape of every other), and a name that differs from
// its values by more than digits.
func TestCSVProbeKeepsCSVWithHeaderLikeValues(t *testing.T) {
	t.Run("header repeated in a minority of rows", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "host,status", "web,200", "host,status", "db,500"))
		assertProbeFields(t, result, map[string][]string{
			"host": {"web", "host", "db"}, "status": {"200", "status", "500"}})
	})
	t.Run("numeric column names", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "2021,2022", "10,20", "30,40"))
		assertProbeFields(t, result, map[string][]string{"2021": {"10", "30"}, "2022": {"20", "40"}})
	})
	t.Run("names with digits", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "p50,p99", "12,40", "p,q"))
		assertProbeFields(t, result, map[string][]string{"p50": {"12", "p"}, "p99": {"40", "q"}})
	})
	t.Run("half of the rows echo the header", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "host,status", "host,1", "db,500", "host,2", "web,200"))
		assertProbeFields(t, result, map[string][]string{
			"host": {"host", "db", "web"}, "status": {"1", "500", "2", "200"}})
	})
	// Fewer than csvProbeMinEchoRows rows are no evidence against a header.
	t.Run("a single row repeating a column name", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "name,enabled", "foo,enabled"))
		assertProbeFields(t, result, map[string][]string{"name": {"foo"}, "enabled": {"enabled"}})
	})
	t.Run("two rows repeating a column name", func(t *testing.T) {
		result := mustProbe(t, "csv", probeSampleOf("a", "name,enabled", "foo,enabled", "bar,enabled"))
		assertProbeFields(t, result, map[string][]string{"name": {"foo", "bar"}, "enabled": {"enabled"}})
	})
	t.Run("three rows repeating a column name are rejected", func(t *testing.T) {
		sample := probeSampleOf("a", "name,enabled", "foo,enabled", "bar,enabled", "baz,enabled")
		if result := mustProbe(t, "csv", sample); !result.Empty() {
			t.Fatalf("result = %+v, want empty", result)
		}
	})
}

// TestCSVProbeKeepsFixtureCorpus measures the plausibility check on the csv
// files of the integration tests (inputs and dmap outfiles): each of them
// must still be probed as csv, with every non-empty header column listed.
func TestCSVProbeKeepsFixtureCorpus(t *testing.T) {
	var files []string
	for _, pattern := range []string{"*.csv.in", "*.csv.expected"} {
		matches, err := filepath.Glob(filepath.Join("..", "..", "..", "integrationtests", pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) < 5 {
		t.Fatalf("only %d csv fixtures found: %v", len(files), files)
	}
	probed := 0
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
		result := mustProbe(t, "csv", probeSampleOf(file, lines...))
		header, err := parseCSVRecord(lines[0])
		if err != nil {
			t.Fatalf("%s: header: %v", file, err)
		}
		if len(header) < csvProbeMinColumns {
			// The documented limit: a csv file of one column is never probed.
			if !result.Empty() {
				t.Errorf("%s: one column, but probed: %v", file, result.Tables)
			}
			continue
		}
		probed++
		for _, column := range header {
			if _, ok := result.Tables[ProbeAnyTable][column]; !ok && csvProbeColumn(column) {
				t.Errorf("%s: column %q not listed, tables %v", file, column, result.Tables)
			}
		}
	}
	if probed < 5 {
		t.Errorf("only %d of %d fixtures have two columns or more", probed, len(files))
	}
	t.Logf("%d of %d csv fixtures probed as csv, the others have one column", probed, len(files))
}

// TestCSVSameShape checks the comparison of a header column with a value:
// equal but for the digits of their digit runs.
func TestCSVSameShape(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"request 1 done", "request 22 done", true},
		{" ok", " ok", true},
		{"took 5ms", "took 170ms", true},
		{"a1", "a", false},
		{"a", "a1", false},
		{"a1b", "a2c", false},
		{"ok", "OK", false},
		{"", "", true},
		{"12", "3", true},
		{"x 1", "x  1", false},
	} {
		if got := csvSameShape(tc.a, tc.b); got != tc.want {
			t.Errorf("csvSameShape(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
