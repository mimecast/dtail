package mapr

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

func TestQueryEmptyStringWhere(t *testing.T) {
	tests := []struct {
		condition string
		empty     bool
		value     bool
	}{
		{`foo eq ""`, true, false},
		{`"" eq foo`, true, false},
		{`foo ne ""`, false, true},
		{`"" ne foo`, false, true},
		{`foo eq "", bar ne ""`, true, false},
		{`foo eq "" and bar ne ""`, true, false},
		{`"" eq foo, "" ne bar`, true, false},
		{`"" eq foo and "" ne bar`, true, false},
		{`"" eq ""`, true, true},
		{`"" ne ""`, false, false},
		{"`where` eq \"\"", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			q, err := NewQuery("select count(*) where "+tt.condition, logging.NopLogger{})
			if err != nil {
				t.Fatal(err)
			}
			for _, input := range []struct {
				value string
				want  bool
			}{{"", tt.empty}, {"value", tt.value}} {
				fields := map[string]string{"foo": input.value, "where": input.value, "bar": "value", "": "not a literal"}
				if got := q.WhereClause(fields); got != input.want {
					t.Errorf("WhereClause(%v) = %v, want %v", fields, got, input.want)
				}
			}
			if len(q.Where) > 1 && q.WhereClause(map[string]string{"foo": "", "bar": ""}) {
				t.Error("WhereClause accepted a failed second condition")
			}
		})
	}
}

func TestQuerySetStringLiteral(t *testing.T) {
	for _, literal := range []string{"", "foo", "maskdigits(foo)", "12", "where"} {
		t.Run(literal, func(t *testing.T) {
			q, err := NewQuery(`select $out set $out = "`+literal+`", $copy = $out`, logging.NopLogger{})
			if err != nil {
				t.Fatal(err)
			}
			fields := map[string]string{literal: "field collision", "foo": "abc123", "$out": "old"}
			if err := q.SetClause(fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"$out", "$copy"} {
				if got, ok := fields[name]; !ok || got != literal {
					t.Errorf("fields[%q] = %q, present %v; want %q", name, got, ok, literal)
				}
			}
		})
	}
}

func TestQueryRejectsInvalidEmptyArguments(t *testing.T) {
	for _, query := range []string{
		" \t\n ",
		`select ""`,
		`select count(*) from ""`,
		`select count(*) group by ""`,
		`select count(*) order by ""`,
		`select count(*) rorder by ""`,
		`select count(*) outfile ""`,
		`select count(*) outfile append ""`,
		`select count(*) logformat ""`,
		`select count(*) interval ""`,
		`select count(*) limit ""`,
		`select count(*) where foo eq`,
		`select count(*) where "" eq`,
		`select count(*) where foo "" bar`,
		`select count(*) where foo == ""`,
		`select count(*) where "" == foo`,
		`select count(*) set $foo =`,
		`select count(*) set "" = foo`,
		`select count(*) set $foo "" bar`,
	} {
		t.Run(query, func(t *testing.T) {
			if q, err := NewQuery(query, logging.NopLogger{}); err == nil {
				t.Fatalf("NewQuery(%q) = %v, want error", query, q)
			}
		})
	}
}

func TestQuerySetFieldAndFunctionSemantics(t *testing.T) {
	for _, tt := range []struct {
		rhs  string
		want string
	}{
		{"foo", "abc123"},
		{"`where`", "keyword field"},
		{"`maskdigits(foo)`", "function field"},
		{"maskdigits(foo)", "abc..."},
	} {
		t.Run(tt.rhs, func(t *testing.T) {
			q, err := NewQuery("select $out set $out = "+tt.rhs, logging.NopLogger{})
			if err != nil {
				t.Fatal(err)
			}
			fields := map[string]string{"foo": "abc123", "where": "keyword field", "maskdigits(foo)": "function field"}
			if err := q.SetClause(fields); err != nil {
				t.Fatal(err)
			}
			if got := fields["$out"]; got != tt.want {
				t.Errorf("fields[$out] = %q, want %q", got, tt.want)
			}
		})
	}
}
