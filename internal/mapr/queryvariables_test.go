package mapr

import (
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

func TestEffectiveLogFormat(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		configured string
		want       string
	}{
		{name: "explicit logformat wins", query: "select $line logformat generickv", want: "generickv"},
		{name: "no from downgrades to generic", query: "select service,sum(bytes) group by service", want: "generic"},
		{name: "from without configured default uses default", query: "from STATS select $line", want: "default"},
		{name: "from honours configured default", query: "from STATS select $line", configured: "mimecast", want: "mimecast"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query, err := NewQuery(tc.query, logging.NopLogger{})
			if err != nil {
				t.Fatalf("NewQuery(%q): %v", tc.query, err)
			}
			if got := query.EffectiveLogFormat(tc.configured); got != tc.want {
				t.Errorf("EffectiveLogFormat(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		})
	}

	// A nil query falls back to the configured default (or "default").
	var nilQuery *Query
	if got := nilQuery.EffectiveLogFormat(""); got != "default" {
		t.Errorf("nil query EffectiveLogFormat(\"\") = %q, want \"default\"", got)
	}
}

func TestReferencedVariables(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "collects $-vars across select, group-by and aggregation",
			query: "from STATS select $service,sum($bytes) group by $service",
			want:  []string{"$bytes", "$service"},
		},
		{
			name:  "barewords are never returned",
			query: "from STATS select service,sum(bytes) group by service",
			want:  nil,
		},
		{
			name:  "where clause $-vars are collected alongside select vars",
			query: "from STATS select $line where $bogus eq foo",
			want:  []string{"$bogus", "$line"},
		},
		{
			name:  "set-defined LHS is excluded but unknown set RHS is included",
			query: "from STATS select $masked set $masked = maskdigits($mystery)",
			want:  []string{"$mystery"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query, err := NewQuery(tc.query, logging.NopLogger{})
			if err != nil {
				t.Fatalf("NewQuery(%q): %v", tc.query, err)
			}
			got := query.ReferencedVariables()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReferencedVariables() = %v, want %v", got, tc.want)
			}
		})
	}
}
