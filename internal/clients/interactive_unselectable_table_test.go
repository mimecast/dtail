package clients

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// unselectableTableCatalog returns the catalog of a probe of two lines of
// the default format: one of the table STATS and one of the table stats,
// which no table name selects (a query's table name is upper-cased).
func unselectableTableCatalog(t *testing.T) catalogSnapshot {
	t.Helper()
	const prefix = "INFO|20211002-071209|1|stats.go:56|8|13|7|0.21|1s|MAPREDUCE:"
	result, err := logformat.Probe("default", "host", logformat.ProbeSample{Lines: []logformat.ProbeLine{
		{Line: prefix + "STATS|upperOnly=1", SourceID: "a.log"},
		{Line: prefix + "stats|lowerOnly=2", SourceID: "a.log"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := newFieldCatalog()
	catalog.store("srv1", catalogTestFiles, protocol.ProbeAnswer{
		DefaultLogFormat: "default",
		FilesSampled:     1,
		Formats: map[string]protocol.ProbeFormat{"default": {
			Tables:         result.Tables,
			TableVariables: result.TableVariables,
			Variables:      result.Variables,
			Note:           result.Note,
		}},
	}, time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	return catalog.snapshot(catalogTestFiles)
}

// candidateNames returns the Tab candidates of line without the blanks,
// commas and quotes the completion inserts around a name.
func candidateNames(t *testing.T, line string, catalog catalogSnapshot) []string {
	t.Helper()
	got := completeInteractive(line, len(line), queryArgs, catalogOf(catalog))
	names := make([]string, 0, len(got.candidates))
	for _, candidate := range got.candidates {
		names = append(names, strings.Trim(candidate, " ,`\"'"))
	}
	return names
}

// TestCompleteQueryOffersFieldsOfUnselectableTable: the fields of the
// lines of a table no table name selects (MAPREDUCE:stats) are read by a
// query without a table, so Tab offers them there; a query naming a table
// is offered only that table's fields, and the table stats is no candidate
// after from (STATS is the only one).
func TestCompleteQueryOffersFieldsOfUnselectableTable(t *testing.T) {
	catalog := unselectableTableCatalog(t)

	withoutTable := candidateNames(t, ":reload --query 'select ", catalog)
	for _, want := range []string{"lowerOnly", "upperOnly"} {
		if !slices.Contains(withoutTable, want) {
			t.Errorf("select without a table offers %q, want %s in it", withoutTable, want)
		}
	}

	withTable := candidateNames(t, ":reload --query 'from STATS select ", catalog)
	if !slices.Contains(withTable, "upperOnly") {
		t.Errorf("select from STATS offers %q, want upperOnly in it", withTable)
	}
	if slices.Contains(withTable, "lowerOnly") {
		t.Errorf("select from STATS offers %q, a field of the table stats", withTable)
	}

	// STATS is the only table, so Tab inserts it instead of listing it.
	line := ":reload --query 'from "
	got := completeInteractive(line, len(line), queryArgs, catalogOf(catalog))
	if !strings.HasPrefix(got.line, line+"STATS") || len(got.candidates) != 0 {
		t.Errorf("from completes to %q with candidates %q, want the one table STATS inserted",
			got.line, got.candidates)
	}
}

// TestFieldsTextShowsFieldsOfUnselectableTable: :fields lists those fields
// under the format's table-less "fields:" entry, not as a table, and the
// note says which lines they come from and how to query them.
func TestFieldsTextShowsFieldsOfUnselectableTable(t *testing.T) {
	catalog := unselectableTableCatalog(t)
	text := fieldsText(catalog, "", time.Date(2026, 11, 19, 12, 0, 1, 0, time.UTC))
	for _, want := range []string{"  fields:\n    lowerOnly", "  table STATS:\n    upperOnly",
		"no table name selects them", "without from (or from .)"} {
		if !strings.Contains(text, want) {
			t.Errorf(":fields = %q, want %q in it", text, want)
		}
	}
	if strings.Contains(text, "table stats:") {
		t.Errorf(":fields = %q, lists the table stats", text)
	}
}
