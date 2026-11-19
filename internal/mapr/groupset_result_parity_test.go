package mapr

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// legacyResultRows preserves the pre-0a allocation, statistics and stable-sort
// path as a differential oracle. Selection/formatting itself is unchanged.
func legacyResultRows(g *GroupSet, q *Query, widths bool) ([]result, []int, error) {
	stats := resultStats{percentageTotals: make(map[string]float64), percentileValues: make(map[string][]float64)}
	for _, set := range g.sets {
		for _, sc := range q.Select {
			value := set.FValues[sc.FieldStorage]
			switch sc.Operation {
			case Percentage:
				stats.percentageTotals[sc.FieldStorage] += value
			case Percentile:
				stats.percentileValues[sc.FieldStorage] = append(stats.percentileValues[sc.FieldStorage], value)
			}
		}
	}
	for _, values := range stats.percentileValues {
		sort.Float64s(values)
	}
	keys := make([]string, 0, len(g.sets))
	for key := range g.sets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var rows []result
	columnWidths := make([]int, len(q.Select))
	for _, key := range keys {
		row := result{groupKey: key}
		for i, sc := range q.Select {
			n, err := g.resultSelect(q, &sc, g.sets[key], &row, &stats)
			if err != nil {
				return rows, columnWidths, err
			}
			if widths {
				columnWidths[i] = max(columnWidths[i], len(sc.FieldStorage), n)
			}
		}
		rows = append(rows, row)
	}
	if q.OrderBy != "" {
		sort.SliceStable(rows, func(i, j int) bool {
			if q.ReverseOrder {
				return rows[i].orderBy < rows[j].orderBy
			}
			return rows[i].orderBy > rows[j].orderBy
		})
	}
	return rows, columnWidths, nil
}

func TestResultRowsMatchLegacy(t *testing.T) {
	fields := []string{"count(v)", "sum(v)", "min(v)", "max(v)", "v", "avg(v)", "len(v)", "percentage(v)", "percentile(v)"}
	selections := append(append([]string{}, fields...), strings.Join(fields, ","), "percentage(v),percentage(v),percentile(v),percentile(v)")
	for _, selection := range selections {
		t.Run(selection, func(t *testing.T) {
			q, err := NewQuery("select "+selection+" from stats group by host", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{0, 1, 10, 21, 127} {
				g := resultParityData(q, size)
				for order := -1; order < len(q.Select); order++ {
					q.OrderBy = ""
					if order >= 0 {
						q.OrderBy = q.Select[order].FieldStorage
					}
					for _, reverse := range []bool{false, true} {
						q.ReverseOrder = reverse
						for _, widths := range []bool{false, true} {
							assertResultRowsMatchLegacy(t, g, q, widths)
						}
					}
				}
			}
		})
	}
}

func resultParityData(q *Query, size int) *GroupSet {
	g := NewGroupSet(nil)
	for i := size - 1; i >= 0; i-- {
		set := g.GetSet(fmt.Sprintf("host-%03d", i))
		set.Samples = i % 4 // Include empty aggregates and ties.
		for _, sc := range q.Select {
			set.FValues[sc.FieldStorage] = float64((i*37)%19 - 9)
			set.SValues[sc.FieldStorage] = []string{"NaN", "+Inf", "-Inf", "2", "2", "-1", "invalid", "-0"}[i%8]
		}
	}
	return g
}

func assertResultRowsMatchLegacy(t *testing.T, g *GroupSet, q *Query, widths bool) {
	t.Helper()
	want, wantWidths, wantErr := legacyResultRows(g, q, widths)
	got, gotWidths, gotErr := g.result(q, widths)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || len(got) != len(want) || !reflect.DeepEqual(gotWidths, wantWidths) {
		t.Fatalf("rows/widths/error mismatch: got %d %v %v, want %d %v %v", len(got), gotWidths, gotErr, len(want), wantWidths, wantErr)
	}
	for i := range want {
		if got[i].groupKey != want[i].groupKey || !reflect.DeepEqual(got[i].values, want[i].values) ||
			(got[i].orderBy != want[i].orderBy && !(math.IsNaN(got[i].orderBy) && math.IsNaN(want[i].orderBy))) {
			t.Fatalf("row%d: got %+v, want %+v (order=%s reverse=%v)", i, got[i], want[i], q.OrderBy, q.ReverseOrder)
		}
	}
}

func TestResultSpecialFloatOrderingMatchesLegacy(t *testing.T) {
	q, err := NewQuery("select sum(v) from stats group by host order by sum(v)", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Stable sort's NaN comparisons are deliberately not a total order. Exercise
	// more than its insertion-sort block size, not just a small sorted example.
	values := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, math.Copysign(0, -1), 3, -4, 3}
	for shift := range values {
		g := NewGroupSet(nil)
		for i := 0; i < 257; i++ {
			g.GetSet(fmt.Sprintf("%04d", i)).FValues["sum(v)"] = values[(i*3+shift)%len(values)]
		}
		for _, reverse := range []bool{false, true} {
			q.ReverseOrder = reverse
			assertResultRowsMatchLegacy(t, g, q, true)
		}
	}
}

func TestResultStorageDoesNotAliasRows(t *testing.T) {
	q, err := NewQuery("select count(v),sum(v) from stats group by host", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := resultParityData(q, 3)
	rows, _, err := g.result(q, true)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := append([]string(nil), rows[1].values...)
	rows[0].values[0] = "changed"
	rows[0].values = append(rows[0].values, "appended")
	if !reflect.DeepEqual(rows[1].values, wantNext) {
		t.Fatal("mutating/appending a row overwrote another row")
	}
	assertResultRowsMatchLegacy(t, g, q, true) // Rendering must not mutate aggregates.
}

func TestResultLimitKeepsAllGroupStatisticsAndWidths(t *testing.T) {
	q, err := NewQuery("select host,percentage(v),percentile(v) from stats group by host order by percentage(v)", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGroupSet(nil)
	for i, host := range []string{"shown", "hidden-with-the-widest-value"} {
		set := g.GetSet(host)
		set.SValues["host"] = host
		set.FValues["percentage(v)"] = float64(3 - 2*i)
		set.FValues["percentile(v)"] = float64(3 - 2*i)
	}
	text, numRows, err := g.Result(q, 1, nil)
	if err != nil || numRows != 2 {
		t.Fatalf("rows=%d error=%v", numRows, err)
	}
	if strings.Contains(text, "hidden-with") || !strings.Contains(text, "75.000000") || !strings.Contains(text, "100.000000") {
		t.Fatalf("limit changed global statistics or leaked hidden row: %q", text)
	}
	if !strings.Contains(text, strings.Repeat(" ", len("hidden-with-the-widest-value")-len("shown"))+"shown") {
		t.Fatalf("hidden row width was omitted: %q", text)
	}
}

func TestResultLimitsAndOutfileMatchLegacy(t *testing.T) {
	q, err := NewQuery("select v,count(v),sum(v) from stats group by host order by count(v)", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := resultParityData(q, 31)
	rows, widths, err := legacyResultRows(g, q, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, queryLimit := range []int{-1, 0, 3, 99} {
		q.Limit = queryLimit
		for _, displayLimit := range []int{-1, 0, 1, 10, 99} {
			limit := displayLimit
			if queryLimit != -1 {
				limit = queryLimit
			}
			var expected strings.Builder
			renderer := PlainResultRenderer()
			g.resultWriteFormattedHeader(q, renderer, &expected, len(q.Select)-1, widths)
			g.resultWriteFormattedHeaderRowSeparator(q, renderer, &expected, len(q.Select)-1, widths)
			g.resultWriteFormattedData(renderer, &expected, len(q.Select)-1, limit, widths, rows)
			text, count, err := g.Result(q, displayLimit, renderer)
			if err != nil || count != 31 || text != expected.String() {
				t.Fatalf("query/display limits%d/%d: rows%d error%v output mismatch", queryLimit, displayLimit, count, err)
			}
		}
		q.Outfile = &Outfile{FilePath: filepath.Join(t.TempDir(), "result.csv")}
		if err := g.WriteResult(q, true); err != nil {
			t.Fatal(err)
		}
		var expected strings.Builder
		if err := g.resultWriteUnformatted(q, rows, &expected, true); err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(q.Outfile.FilePath)
		if err != nil || string(actual) != expected.String() {
			t.Fatalf("CSV mismatch (limit%d): err%v", queryLimit, err)
		}
		queryFile, err := os.ReadFile(q.Outfile.FilePath + ".query")
		if err != nil || string(queryFile) != q.RawQuery {
			t.Fatalf("query file changed: %q err%v", queryFile, err)
		}
	}
}

func TestResultInvalidAggregationStillFailsWithZeroLimit(t *testing.T) {
	q := &Query{Select: []selectCondition{{Operation: AggregateOperation(-1), FieldStorage: "invalid"}}, Limit: 0}
	g := NewGroupSet(nil)
	g.GetSet("bad")
	assertResultRowsMatchLegacy(t, g, q, true)
	text, count, err := g.Result(q, 0, nil)
	if err == nil || text != "" || count != 0 {
		t.Fatalf("invalid operation hidden by zero limit: text%q count%d err%v", text, count, err)
	}
}
