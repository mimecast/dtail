package mapr

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// TestParseQueryRejectsDuplicateSelect checks that a query selecting one
// storage key twice fails to parse with an error naming it. Such a query used
// to aggregate every line into the shared key once per select field, e.g.
// count(x),count(x) counted each line twice.
func TestParseQueryRejectsDuplicateSelect(t *testing.T) {
	tests := []struct {
		name, query, want string
	}{
		{"count twice", "select count(x),count(x) from STATS",
			"'select' clause has 'count(x)' more than once"},
		{"sum twice with space", "select sum($x), sum($x) from STATS group by $h",
			"'select' clause has 'sum($x)' more than once"},
		{"avg twice", "select avg(x),avg(x) from STATS",
			"'select' clause has 'avg(x)' more than once"},
		{"not adjacent", "select count(x),sum(x),min(x),count(x) from STATS",
			"'select' clause has 'count(x)' more than once"},
		{"percentile twice", "select percentile(x),percentile(x) from STATS",
			"'select' clause has 'percentile(x)' more than once"},
		{"plain field twice", "select x,x from STATS",
			"'select' clause has 'x' more than once"},
		{"plain and quoted field", "select x,`x` from STATS",
			"'select' clause has 'x' more than once"},
		{"quoted field named like aggregation first", "select `count(x)`,count(x) from STATS",
			"'select' clause has both the field `count(x)` and the aggregation count(x)"},
		{"aggregation first", "select count(x),`count(x)` from STATS",
			"'select' clause has both the field `count(x)` and the aggregation count(x)"},
		{"quoted field named like last", "select last(x),`last(x)` from STATS",
			"'select' clause has both the field `last(x)` and the aggregation last(x)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := NewQuery(tt.query, logging.NopLogger{})
			if err == nil {
				t.Fatalf("NewQuery(%q) = %v, want an error", tt.query, q)
			}
			if !strings.Contains(err.Error(), invalidQuery+tt.want) {
				t.Fatalf("NewQuery(%q) error = %q, want it to contain %q",
					tt.query, err, invalidQuery+tt.want)
			}
		})
	}
}

// TestParseQueryAcceptsDistinctSelect checks that select fields with distinct
// storage keys still parse, also when they aggregate the same field in
// different ways or name different fields that look alike.
func TestParseQueryAcceptsDistinctSelect(t *testing.T) {
	queries := []string{
		"select count(x),sum(x),avg(x),min(x),max(x),last(x),len(x),percentage(x)," +
			"percentile(x),x from STATS",
		"select count($x),count(x),count(`x`) from STATS",
		"select `count(x)`,count($x),`sum(x)` from STATS",
		"select count(x) from STATS group by x order by count(x)",
		// The group by field selected next to an aggregation of it.
		"select x,count(x) from STATS group by x",
		"select $hostname,count($hostname),sum(lat) from STATS group by $hostname",
		// A field used in the where clause and in the select clause.
		"select lat,count(lat) from STATS where lat > 10 group by lat",
		"select status,count(lat) from STATS where status eq ok group by status",
	}
	for _, query := range queries {
		q, err := NewQuery(query, logging.NopLogger{})
		if err != nil {
			t.Errorf("NewQuery(%q) error = %v", query, err)
			continue
		}
		seen := make(map[string]bool, len(q.Select))
		for _, sc := range q.Select {
			if seen[sc.FieldStorage] {
				t.Errorf("NewQuery(%q) has storage key %q twice", query, sc.FieldStorage)
			}
			seen[sc.FieldStorage] = true
		}
	}
}
