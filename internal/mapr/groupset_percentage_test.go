package mapr

import (
	"strconv"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

func TestGroupSetResultPercentageAndPercentile(t *testing.T) {
	query, queryErr := NewQuery("select percentage(value),percentile(value) from stats group by host order by percentage(value)", logging.NopLogger{})
	if queryErr != nil {
		t.Fatalf("Unable to parse query: %v", queryErr)
	}

	groupSet := NewGroupSet(logging.NopLogger{})

	setA := groupSet.GetSet("host-a")
	if err := setA.Aggregate("percentage(value)", Percentage, "10", false); err != nil {
		t.Fatalf("Unable to aggregate percentage for host-a: %v", err)
	}
	if err := setA.Aggregate("percentile(value)", Percentile, "10", false); err != nil {
		t.Fatalf("Unable to aggregate percentile for host-a: %v", err)
	}

	setB := groupSet.GetSet("host-b")
	if err := setB.Aggregate("percentage(value)", Percentage, "30", false); err != nil {
		t.Fatalf("Unable to aggregate percentage for host-b: %v", err)
	}
	if err := setB.Aggregate("percentile(value)", Percentile, "30", false); err != nil {
		t.Fatalf("Unable to aggregate percentile for host-b: %v", err)
	}

	setC := groupSet.GetSet("host-c")
	if err := setC.Aggregate("percentage(value)", Percentage, "20", false); err != nil {
		t.Fatalf("Unable to aggregate percentage for host-c: %v", err)
	}
	if err := setC.Aggregate("percentile(value)", Percentile, "20", false); err != nil {
		t.Fatalf("Unable to aggregate percentile for host-c: %v", err)
	}

	rows, _, err := groupSet.result(query, false)
	if err != nil {
		t.Fatalf("Unable to build result rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("Expected 3 result rows, got %d", len(rows))
	}

	if rows[0].groupKey != "host-b" {
		t.Fatalf("Expected rows to be ordered by percentage descending, first row=%s", rows[0].groupKey)
	}

	valuesByGroup := map[string][]float64{}
	for _, row := range rows {
		parsedValues := make([]float64, 0, len(row.values))
		for _, value := range row.values {
			parsedValue, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatalf("Unable to parse result value %q: %v", value, err)
			}
			parsedValues = append(parsedValues, parsedValue)
		}
		valuesByGroup[row.groupKey] = parsedValues
	}

	assertAlmostEqual(t, valuesByGroup["host-a"][0], 16.6666666667, "host-a percentage")
	assertAlmostEqual(t, valuesByGroup["host-a"][1], 33.3333333333, "host-a percentile")
	assertAlmostEqual(t, valuesByGroup["host-b"][0], 50.0, "host-b percentage")
	assertAlmostEqual(t, valuesByGroup["host-b"][1], 100.0, "host-b percentile")
	assertAlmostEqual(t, valuesByGroup["host-c"][0], 33.3333333333, "host-c percentage")
	assertAlmostEqual(t, valuesByGroup["host-c"][1], 66.6666666667, "host-c percentile")
}

func TestGroupSetPercentageReturnsZeroWhenTotalIsZero(t *testing.T) {
	query, queryErr := NewQuery("select percentage(value) from stats group by host", logging.NopLogger{})
	if queryErr != nil {
		t.Fatalf("Unable to parse query: %v", queryErr)
	}

	groupSet := NewGroupSet(logging.NopLogger{})
	for _, host := range []string{"host-a", "host-b"} {
		set := groupSet.GetSet(host)
		if err := set.Aggregate("percentage(value)", Percentage, "0", false); err != nil {
			t.Fatalf("Unable to aggregate percentage for %s: %v", host, err)
		}
	}

	rows, _, err := groupSet.result(query, false)
	if err != nil {
		t.Fatalf("Unable to build result rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Expected 2 result rows, got %d", len(rows))
	}
	for _, row := range rows {
		if len(row.values) != 1 {
			t.Fatalf("Expected one result value, got %d for %s", len(row.values), row.groupKey)
		}
		value, err := strconv.ParseFloat(row.values[0], 64)
		if err != nil {
			t.Fatalf("Unable to parse percentage result %q: %v", row.values[0], err)
		}
		assertAlmostEqual(t, value, 0.0, row.groupKey+" percentage")
	}
}

// TestGroupSetPercentileSkipsGroupsLackingField verifies that a group none of
// whose lines had the percentile field takes no part in the ranking and shows
// 0. It used to be ranked as 0, which shifted the other groups' ranks: with
// the groups lat=-5, no lat and lat=-1 the result was 33.3, 100.0 and 66.7.
func TestGroupSetPercentileSkipsGroupsLackingField(t *testing.T) {
	query, err := NewQuery("select percentile(lat),count(status) from stats group by host order by percentile(lat)",
		logging.NopLogger{})
	if err != nil {
		t.Fatalf("Unable to parse query: %v", err)
	}
	groupSet := NewGroupSet(logging.NopLogger{})
	for host, lat := range map[string]string{"host-a": "-5", "host-b": "", "host-c": "-1"} {
		set := groupSet.GetSet(host)
		if lat != "" {
			if aggErr := set.Aggregate("percentile(lat)", Percentile, lat, false); aggErr != nil {
				t.Fatalf("Unable to aggregate percentile for %s: %v", host, aggErr)
			}
		}
		if aggErr := set.Aggregate("count(status)", Count, "ok", false); aggErr != nil {
			t.Fatalf("Unable to aggregate count for %s: %v", host, aggErr)
		}
	}

	rows, _, err := groupSet.result(query, false)
	if err != nil {
		t.Fatalf("Unable to build result rows: %v", err)
	}
	want := []struct {
		host       string
		percentile string
	}{{"host-c", "100.000000"}, {"host-a", "50.000000"}, {"host-b", "0.000000"}}
	if len(rows) != len(want) {
		t.Fatalf("Expected %d result rows, got %d", len(want), len(rows))
	}
	for i, w := range want {
		if rows[i].groupKey != w.host || rows[i].values[0] != w.percentile || rows[i].values[1] != "1" {
			t.Errorf("row %d = %s %v, want %s [%s 1]", i, rows[i].groupKey, rows[i].values, w.host, w.percentile)
		}
	}
}

func TestPercentileRank(t *testing.T) {
	sortedValues := []float64{10, 20, 30}

	tests := []struct {
		name     string
		value    float64
		expected float64
	}{
		{name: "below minimum", value: 5, expected: 0},
		{name: "first bucket", value: 10, expected: 33.3333333333},
		{name: "middle bucket", value: 20, expected: 66.6666666667},
		{name: "maximum", value: 30, expected: 100},
		{name: "above maximum", value: 40, expected: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertAlmostEqual(t, percentileRank(tt.value, sortedValues), tt.expected, tt.name)
		})
	}

	assertAlmostEqual(t, percentileRank(10, []float64{10, 10, 30}), 66.6666666667, "duplicate percentile rank")
	if got := percentileRank(10, nil); got != 0 {
		t.Fatalf("Expected empty percentile input to return 0, got %f", got)
	}
}

func assertAlmostEqual(t *testing.T, got, expected float64, label string) {
	t.Helper()
	const tolerance = 0.0001

	diff := got - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Fatalf("Unexpected %s: got=%f expected=%f tolerance=%f", label, got, expected, tolerance)
	}
}
