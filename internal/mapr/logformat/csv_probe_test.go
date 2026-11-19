package logformat

import (
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
