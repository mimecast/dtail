package mapr

import (
	"errors"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

func TestNewQueryRejectsEmptyQuery(t *testing.T) {
	query, err := NewQuery("", logging.NopLogger{})
	if query != nil {
		t.Fatalf("NewQuery returned query %#v with an empty input", query)
	}
	if !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("NewQuery error = %v, want ErrEmptyQuery", err)
	}
}

func TestParseQueryOutfile(t *testing.T) {
	queryStr := "select foo from bar outfile \"baz.csv\""

	q, err := NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Errorf("Query parse error: %s\n%v: %v", queryStr, q, err)
	}

	if q.Outfile == nil {
		t.Errorf("Expected non-nil outfile: %s\n%v", queryStr, q)
	}

	if q.Outfile.FilePath != "baz.csv" {
		t.Errorf("Expected \"baz.csv\" as outfile file path: %s\n%v", queryStr, q)
	}

	if q.Outfile.AppendMode {
		t.Errorf("Expected append mode of outfile to be false: %s\n%v", queryStr, q)
	}
}

func TestParseQueryOutfileAppend(t *testing.T) {
	queryStr := "select foo from bar outfile append \"baz.csv\""

	q, err := NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Errorf("Query parse error: %s\n%v: %v", queryStr, q, err)
	}

	if q.Outfile == nil {
		t.Errorf("Expected non-nil outfile: %s\n%v", queryStr, q)
	}

	if q.Outfile.FilePath != "baz.csv" {
		t.Errorf("Expected \"baz.csv\" as outfile file path: %s\n%v", queryStr, q)
	}

	if !q.Outfile.AppendMode {
		t.Errorf("Expected append mode of outfile to be true: %s\n%v", queryStr, q)
	}
}

func TestParseQuerySimple(t *testing.T) {
	errorQueries := []string{
		"select",
		"select foo from",
		"select foo from bar where baz",
		"select foo from bar where baz <",
		"select foo from bar where baz < 100 bay eq 12 group",
		"select foo from bar where baz < 100 bay eq 12 group by foo order by",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo limit",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo limit set foo = bar;",
	}
	okQueries := []string{"select foo from bar",
		"select foo from bar where",
		"select foo from bar where baz < 100 bay eq 12",
		"select foo from bar where baz < 100, bay eq 12",
		"select foo from bar where baz < 100 and bay eq 12",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo limit 23",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo limit 23 outfile \"result.csv\"",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo limit 23 outfile append \"result.csv\"",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz " +
			"order by foo limit 23 outfile append \"result.csv\" " +
			"set $foo = maskdigits(bar), $baz = 12, $bay = $foo;",
	}

	for _, queryStr := range errorQueries {
		q, err := NewQuery(queryStr, logging.NopLogger{})
		if err == nil {
			t.Errorf("Expected a parse error: %s\n%v", queryStr, q)
			continue
		}
	}

	for _, queryStr := range okQueries {
		_, err := NewQuery(queryStr, logging.NopLogger{})
		if err != nil {
			t.Errorf("%s: %s", err.Error(), queryStr)
			continue
		}
	}
}

func TestParseQueryDeep(t *testing.T) {
	dialects := []string{
		"select s1, `from`, count(s3) from table where w1 == 2 and w2 eq " +
			"\"free beer\" group by g1, g2 order by count(s3) interval 10 limit 23 " +
			"set $foo = maskdigits(bar), $baz = 12, $bay = $foo logformat generic",

		"SELECT s1, `from`, count(s3) FROM table WHERE w1 == 2 AND w2 EQ " +
			"\"free beer\" GROUP g1, g2 ORDER count(s3) INTERVAL 10 LIMIT 23 " +
			"SET $foo = maskdigits(bar), $baz = 12, $bay = $foo LOGFORMAT generic",
	}

	for _, queryStr := range dialects {
		q, err := NewQuery(queryStr, logging.NopLogger{})
		if err != nil {
			t.Errorf("%s: %s", err.Error(), queryStr)
		}
		t.Log(q)

		// 'select' clause
		if len(q.Select) != 3 {
			t.Errorf("Expected three elements in 'select' clause but got '%v': %s\n%v",
				q.Select, queryStr, q)
		}
		if q.Select[0].Field != "s1" {
			t.Errorf("Expected 's1' as first element in 'select' clause but got '%v': %s\n%v",
				q.Select[0].Field, queryStr, q)
		}
		if q.Select[0].Operation != Last {
			t.Errorf("Expected 'last' as aggregation function of first element in "+
				"'select' clause but got '%v': %s\n%v", q.Select[0].Operation, queryStr, q)
		}
		if q.Select[1].Field != "from" {
			t.Errorf("Expected 'from' as second element in 'select' clause but got "+
				"'%v': %s\n%v", q.Select[1].Field, queryStr, q)
		}
		if q.Select[1].Operation != Last {
			t.Errorf("Expected 'last' as aggregation function of second element in "+
				"'select' clause but got '%v': %s\n%v", q.Select[1].Operation, queryStr, q)
		}
		if q.Select[2].Field != "s3" {
			t.Errorf("Expected 's3' as third element in 'select' clause but got "+
				"'%v': %s\n%v", q.Select[2].Field, queryStr, q)
		}
		if q.Select[2].Operation != Count {
			t.Errorf("Expected 'count' as aggregation function of third  element in "+
				"'select' clause but got '%v': %s\n%v", q.Select[2].Operation, queryStr, q)
		}
		if q.Select[2].FieldStorage != "count(s3)" {
			t.Errorf("Expected 'count(s3)' as third element's storage in 'select' "+
				"clause but got '%v': %s\n%v", q.Select[2].FieldStorage, queryStr, q)
		}

		// 'from' clause
		if q.Table != "TABLE" {
			t.Errorf("Expected 'TABLE' in 'from' clause but got '%v': %s\n%v",
				q.Table, queryStr, q)
		}

		// 'where' clause
		if len(q.Where) != 2 {
			t.Errorf("Expected two elements in 'where' clause but got '%v': %s\n%v",
				q.Where, queryStr, q)
		}
		if q.Where[0].lString != "w1" {
			t.Errorf("Expected w1 as first element in 'where' clause but got '%v': %s\n%v",
				q.Where[0].lString, queryStr, q)
		}
		if q.Where[0].Operation != FloatEq {
			t.Errorf("Expected FloatEq operation in first 'where' condition but got "+
				"'%v': %s\n%v", q.Where[0].Operation, queryStr, q)
		}
		if q.Where[0].rFloat != 2 {
			t.Errorf("Expected '2' as float argument in first 'where' condition but "+
				"got '%v': %s\n%v", q.Where[0].rFloat, queryStr, q)
		}
		if q.Where[1].lString != "w2" {
			t.Errorf("Expected w2 as second element in 'where' clause but got '%v': "+
				"%s\n%v", q.Where[1].lString, queryStr, q)
		}
		if q.Where[1].Operation != StringEq {
			t.Errorf("Expected StringEq operation in second 'where' condition but got "+
				"'%v': %s\n%v", q.Where[0].Operation, queryStr, q)
		}
		if q.Where[1].rString != "free beer" {
			t.Errorf("Expected 'free beer' as string argument in second 'where' "+
				"condition but got '%v': %s\n%v", q.Where[0].rString, queryStr, q)
		}

		// 'group by' clause
		if len(q.GroupBy) != 2 {
			t.Errorf("Expected two elements in 'group by' clause but got '%v': %s\n%v",
				q.GroupBy, queryStr, q)
		}
		if q.GroupBy[0] != "g1" {
			t.Errorf("Expected 'g1' as first element in 'group by' clause but got "+
				"'%v': %s\n%v", q.GroupBy[0], queryStr, q)
		}
		if q.GroupBy[1] != "g2" {
			t.Errorf("Expected 'g2' as second element in 'group by' clause but got "+
				"'%v': %s\n%v", q.GroupBy[1], queryStr, q)
		}
		if q.GroupKey != "g1,g2" {
			t.Errorf("Expected 'g1,g2' as group key in 'group by' clause but got "+
				"'%v': %s\n%v", q.GroupKey, queryStr, q)
		}

		// 'order by' clause
		if q.OrderBy != "count(s3)" {
			t.Errorf("Expected 'count(s3)' as element in 'order by' clause but got "+
				"'%v': %s\n%v", q.OrderBy, queryStr, q)
		}

		// 'interval' clause
		if q.Interval != time.Second*time.Duration(10) {
			t.Errorf("Expected '10s' as duration 'interval' clause but got '%v': %s\n%v",
				q.Interval, queryStr, q)
		}

		// 'limit' clause
		if q.Limit != 23 {
			t.Errorf("Expected '23' as limit in 'limit' clause but got '%v': %s\n%v",
				q.Limit, queryStr, q)
		}

		// 'set' clause
		if q.Set[0].lString != "$foo" {
			t.Errorf("Expected '$foo' lvalue in first 'set' condition clause but got "+
				"'%v': %s\n%v", q.Set[0].lString, queryStr, q)
		}
		if q.Set[0].rString != "bar" {
			t.Errorf("Expected 'bar' rvalue in first 'set' condition clause but got "+
				"'%v': %s\n%v", q.Set[0].rString, queryStr, q)
		}
		if q.Set[1].lString != "$baz" {
			t.Errorf("Expected '$baz' lvalue in second 'set' condition clause but got "+
				"'%v': %s\n%v", q.Set[1].lString, queryStr, q)
		}
		if q.Set[1].rString != "12" {
			t.Errorf("Expected '12' rvalue in second 'set' condition clause but got "+
				"'%v': %s\n%v", q.Set[1].rString, queryStr, q)
		}
		if q.Set[2].lString != "$bay" {
			t.Errorf("Expected '$bay' lvalue in third 'set' condition clause but got "+
				"'%v': %s\n%v", q.Set[2].lString, queryStr, q)
		}
		if q.Set[2].rString != "$foo" {
			t.Errorf("Expected '$foo' rvalue in third 'set' condition clause but got "+
				"'%v': %s\n%v", q.Set[2].rString, queryStr, q)
		}

		// 'logformat' clause
		if q.LogFormat != "generic" {
			t.Errorf("Expected 'generic' logformat got '%v': %s\n%v",
				q.LogFormat, queryStr, q)
		}
	}
}

func TestQuotedSelectCondition(t *testing.T) {
	queryStr := "select `count($foo)`, foo, $foo, count($foo) logformat csv"

	q, err := NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Errorf("Query parse error: %s\n%v: %v", queryStr, q, err)
	}
	if len(q.Select) != 4 {
		t.Errorf("Expected three elements in 'select' clause but got '%v': %s\n%v",
			q.Select, queryStr, q)
	}

	if q.Select[0].Field != "count($foo)" {
		t.Errorf("Expected 'num($foo)' as first element in 'select' clause but got '%v': %s\n%v",
			q.Select[0].Field, queryStr, q)
	}
	if q.Select[0].Operation != Last {
		t.Errorf("Expected 'Last' as aggregation function of first element in "+
			"'select' clause but got '%v': %s\n%v", q.Select[0].Operation, queryStr, q)
	}

	if q.Select[1].Field != "foo" {
		t.Errorf("Expected 'foo' as first element in 'select' clause but got '%v': %s\n%v",
			q.Select[1].Field, queryStr, q)
	}
	if q.Select[2].Field != "$foo" {
		t.Errorf("Expected '$foo' as first element in 'select' clause but got '%v': %s\n%v",
			q.Select[2].Field, queryStr, q)
	}

	if q.Select[3].Field != "$foo" {
		t.Errorf("Expected '$foo' as first element in 'select' clause but got '%v': %s\n%v",
			q.Select[3].Field, queryStr, q)
	}
	if q.Select[3].Operation != Count {
		t.Errorf("Expected 'count' as aggregation function of thourth element in "+
			"'select' clause but got '%v': %s\n%v", q.Select[3].Operation, queryStr, q)
	}
}

// TestCsvLogformatDefaultTable verifies that a query using "logformat csv"
// without an explicit FROM clause gets the default table "." applied after
// parsing. This is a regression test for a bug where the CSV check ran
// before q.parse(), so LogFormat was always empty and Table was never set.
func TestCsvLogformatDefaultTable(t *testing.T) {
	// Without an explicit FROM, the CSV logformat should default to table "."
	// meaning "process all lines without file filtering".
	queryStr := "select foo logformat csv"

	q, err := NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Query parse error: %s\n%v: %v", queryStr, q, err)
	}

	if q.LogFormat != "csv" {
		t.Errorf("Expected LogFormat 'csv' but got '%v': %s\n%v", q.LogFormat, queryStr, q)
	}

	// The CSV default table must be "." when no FROM clause is provided.
	if q.Table != "." {
		t.Errorf("Expected Table '.' for CSV logformat without FROM clause but got '%v': %s\n%v",
			q.Table, queryStr, q)
	}
}

// TestCsvLogformatExplicitTableNotOverridden verifies that an explicit FROM
// clause is preserved when using "logformat csv"; the default "." must not
// override a user-supplied table name.
func TestCsvLogformatExplicitTableNotOverridden(t *testing.T) {
	queryStr := "select foo from myfile logformat csv"

	q, err := NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Query parse error: %s\n%v: %v", queryStr, q, err)
	}

	if q.LogFormat != "csv" {
		t.Errorf("Expected LogFormat 'csv' but got '%v': %s\n%v", q.LogFormat, queryStr, q)
	}

	// Explicit FROM value must be preserved and not replaced with ".".
	if q.Table != "MYFILE" {
		t.Errorf("Expected Table 'MYFILE' for CSV logformat with explicit FROM but got '%v': %s\n%v",
			q.Table, queryStr, q)
	}
}

func TestParseQueryPercentageAndPercentile(t *testing.T) {
	queryStr := "select percentage($value),percentile($value) from stats group by $host order by percentile($value)"

	q, err := NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Errorf("Query parse error: %s\n%v: %v", queryStr, q, err)
	}
	if len(q.Select) != 2 {
		t.Fatalf("Expected two elements in 'select' clause but got '%v': %s\n%v",
			q.Select, queryStr, q)
	}

	if q.Select[0].Operation != Percentage {
		t.Errorf("Expected 'percentage' as first select aggregation but got '%v': %s\n%v",
			q.Select[0].Operation, queryStr, q)
	}
	if q.Select[0].FieldStorage != "percentage($value)" {
		t.Errorf("Expected percentage field storage but got '%v': %s\n%v",
			q.Select[0].FieldStorage, queryStr, q)
	}

	if q.Select[1].Operation != Percentile {
		t.Errorf("Expected 'percentile' as second select aggregation but got '%v': %s\n%v",
			q.Select[1].Operation, queryStr, q)
	}
	if q.Select[1].FieldStorage != "percentile($value)" {
		t.Errorf("Expected percentile field storage but got '%v': %s\n%v",
			q.Select[1].FieldStorage, queryStr, q)
	}

	if q.OrderBy != "percentile($value)" {
		t.Errorf("Expected order by percentile($value) but got '%v': %s\n%v",
			q.OrderBy, queryStr, q)
	}
}
