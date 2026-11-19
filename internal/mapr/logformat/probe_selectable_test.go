package logformat

import (
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// lowerTableLine is a line of the default format whose table name holds
// lower-case letters: no table name selects it, as a query's table name is
// upper-cased.
const lowerTableLine = "INFO|20211002-071209|1|stats.go:56|8|13|7|0.21|1s|MAPREDUCE:stats|lowerOnly=1"

// TestProbeListsOnlySelectableTables: every table the default format's
// probe lists is one a query can read. `from stats` is parsed as STATS,
// whose lines are matched by that exact name, so a MAPREDUCE:stats line is
// in no table a query can name: the probe lists no table "stats", which
// would complete to nothing. A query without a table still reads the line,
// so its fields and $variables are listed under no table (ProbeAnyTable),
// and the note says how to query them.
func TestProbeListsOnlySelectableTables(t *testing.T) {
	result := mustProbe(t, "default", probeSampleOf("a.log", lowerTableLine, probeStatsLine))

	for table := range result.Tables {
		if table == ProbeAnyTable {
			continue
		}
		query, err := mapr.NewQuery("select count($line) from "+table, logging.NopLogger{})
		if err != nil {
			t.Fatalf("listed table %q: %v", table, err)
		}
		if query.Table != table {
			t.Errorf("listed table %q is read as %q by a query", table, query.Table)
		}
	}
	if got := sortedKeys(result.Tables); !slices.Equal(got, []string{ProbeAnyTable, "STATS"}) {
		t.Fatalf("tables %q, want no table and STATS", got)
	}
	if _, ok := result.Tables["STATS"]["lowerOnly"]; ok {
		t.Error("STATS lists a field of the MAPREDUCE:stats line")
	}
	if got := result.Tables[ProbeAnyTable]["lowerOnly"]; !slices.Equal(got, []string{"1"}) {
		t.Errorf("lowerOnly without a table = %v, want its value 1", got)
	}
	if _, ok := result.TableVariables["stats"]; ok {
		t.Error("table variables list the unselectable table stats")
	}
	if got := result.TableVariables[ProbeAnyTable]["$caller"]; !slices.Equal(got, []string{"stats.go:56"}) {
		t.Errorf("$caller without a table = %v, want the caller of the stats line", got)
	}
	for _, want := range []string{`"stats"`, "no table name selects them", "without from (or from .)"} {
		if !strings.Contains(result.Note, want) {
			t.Errorf("note %q, want %q in it", result.Note, want)
		}
	}
	if strings.Contains(result.Note, "no from clause") {
		t.Errorf("note %q denies that from . reads the lines", result.Note)
	}
}

// TestProbeNoNoteWithoutUnselectableTables: the note is only set when a
// line has such a table.
func TestProbeNoNoteWithoutUnselectableTables(t *testing.T) {
	result := mustProbe(t, "default", probeSampleOf("a.log", probeStatsLine, probeOrdersLine))
	if result.Note != "" {
		t.Errorf("note %q, want none", result.Note)
	}
	if got := sortedKeys(result.Tables); !slices.Equal(got, []string{"ORDERS", "STATS"}) {
		t.Errorf("tables %v, want [ORDERS STATS]", got)
	}
}

// TestProbeTableScannerSkipsUnselectableTables: the scanner lists no table
// a query cannot name (their lines count for the entry of no table), and
// the table probe of the upper-cased name (what `from stats` asks for)
// keeps no line of the lower-case table, also not one that holds the name
// as a word.
func TestProbeTableScannerSkipsUnselectableTables(t *testing.T) {
	scanner := NewProbeTableScanner([]string{"default"}, "host")
	for _, line := range []string{lowerTableLine, probeStatsLine,
		strings.Replace(lowerTableLine, "MAPREDUCE:stats", "MAPREDUCE:Stats", 1)} {
		scanner.Scan([]byte(line))
	}
	if got := scanner.Tables("default"); !slices.Equal(got, []string{ProbeAnyTable, "STATS"}) {
		t.Fatalf("scanned tables %q, want no table and STATS", got)
	}
	matches := scanner.Matcher("STATS")
	if matches([]byte(lowerTableLine)) || matches([]byte(lowerTableLine+"|peer=STATS")) {
		t.Error("the table probe of STATS keeps a MAPREDUCE:stats line")
	}
	if !matches([]byte(probeStatsLine)) {
		t.Error("the table probe of STATS drops a MAPREDUCE:STATS line")
	}
}
