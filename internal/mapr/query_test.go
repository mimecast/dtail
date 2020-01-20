package mapr

import (
	"testing"
	"time"
)

func TestParseQuerySimple(t *testing.T) {
	errorQueries := []string{
		"select",
		"select foo",
		"select foo from",
		"select foo from bar where baz",
		"select foo from bar where baz <",
		"select foo from bar where baz < 100 bay eq 12 group",
		"select foo from bar where baz < 100 bay eq 12 group by foo order by",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz order by foo limit",
	}
	okQueries := []string{"select foo from bar",
		"select foo from bar where",
		"select foo from bar where baz < 100 bay eq 12",
		"select foo from bar where baz < 100, bay eq 12",
		"select foo from bar where baz < 100 and bay eq 12",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz order by foo",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz order by foo limit 23",
		"select foo from bar where baz < 100 bay eq 12 group by foo, bar, baz order by foo limit 23 outfile \"result.csv\"",
	}

	for _, queryStr := range errorQueries {
		q, err := NewQuery(queryStr)
		if err == nil {
			t.Errorf("Expected a parse error: %s\n%v", queryStr, q)
			continue
		}
	}

	for _, queryStr := range okQueries {
		_, err := NewQuery(queryStr)
		if err != nil {
			t.Errorf("%s: %s", err.Error(), queryStr)
			continue
		}
	}
}

func TestParseQueryDeep(t *testing.T) {
	dialects := []string{
		"select s1, `from`, count(s3) from table where w1 == 2 and w2 eq \"free beer\" group by g1, g2 order by count(s3) interval 10 limit 23",
		"SELECT s1, `from` COUNT(s3) FROM table WHERE w1 == 2 AND w2 eq \"free beer\" GROUP g1, g2 ORDER count(s3) INTERVAL 10 LIMIT 23",
		"select s1, `from` count(s3) from table where w1 == 2 and w2 eq \"free beer\" group by g1, g2 order by count(s3) interval 10 limit 23",
		"sElEct s1, `from` coUnt(s3) from taBle where w1 == 2 aNd w2 eq \"free beer\" Group By g1, g2 order bY count(s3) intervaL 10 LiMiT 23",
		"SELECT s1 `from` COUNT(s3) FROM table WHERE w1 == 2 AND w2 eq \"free beer\" GROUP BY g1 g2 ORDER BY count(s3) INTERVAL 10 LIMIT 23",
		"select s1 `from` count(s3) from table where w1 == 2 w2 eq \"free beer\" group g1 g2 order count(s3) interval 10 limit 23",
		"limit 23 interval 10 order count(s3) group g1 g2 where w1 == 2 w2 eq \"free beer\" from table select s1 `from` count(s3)",
	}

	for _, queryStr := range dialects {
		q, err := NewQuery(queryStr)
		if err != nil {
			t.Errorf("%s: %s", err.Error(), queryStr)
		}

		// 'select' clause
		if len(q.Select) != 3 {
			t.Errorf("Expected three elements in 'select' clause but got '%v': %s\n%v", q.Select, queryStr, q)
		}

		if q.Select[0].Field != "s1" {
			t.Errorf("Expected 's1' as first element in 'select' clause but got '%v': %s\n%v", q.Select[0].Field, queryStr, q)
		}
		if q.Select[0].Operation != Last {
			t.Errorf("Expected 'last' as aggregation function of first element in 'select' clause but got '%v': %s\n%v", q.Select[0].Operation, queryStr, q)
		}

		if q.Select[1].Field != "from" {
			t.Errorf("Expected 'from' as second element in 'select' clause but got '%v': %s\n%v", q.Select[1].Field, queryStr, q)
		}
		if q.Select[1].Operation != Last {
			t.Errorf("Expected 'last' as aggregation function of second element in 'select' clause but got '%v': %s\n%v", q.Select[1].Operation, queryStr, q)
		}

		if q.Select[2].Field != "s3" {
			t.Errorf("Expected 's3' as third element in 'select' clause but got '%v': %s\n%v", q.Select[2].Field, queryStr, q)
		}
		if q.Select[2].Operation != Count {
			t.Errorf("Expected 'count' as aggregation function of third  element in 'select' clause but got '%v': %s\n%v", q.Select[2].Operation, queryStr, q)
		}
		if q.Select[2].FieldStorage != "count(s3)" {
			t.Errorf("Expected 'count(s3)' as third element's storage in 'select' clause but got '%v': %s\n%v", q.Select[2].FieldStorage, queryStr, q)
		}

		// 'from' clause
		if q.Table != "TABLE" {
			t.Errorf("Expected 'TABLE' in 'from' clause but got '%v': %s\n%v", q.Table, queryStr, q)
		}

		// 'where' clause
		if len(q.Where) != 2 {
			t.Errorf("Expected two elements in 'where' clause but got '%v': %s\n%v", q.Where, queryStr, q)
		}
		if q.Where[0].lString != "w1" {
			t.Errorf("Expected w1 as first element in 'where' clause but got '%v': %s\n%v", q.Where[0].lString, queryStr, q)
		}
		if q.Where[0].Operation != FloatEq {
			t.Errorf("Expected FloatEq operation in first 'where' condition but got '%v': %s\n%v", q.Where[0].Operation, queryStr, q)
		}
		if q.Where[0].rFloat != 2 {
			t.Errorf("Expected '2' as float argument in first 'where' condition but got '%v': %s\n%v", q.Where[0].rFloat, queryStr, q)
		}
		if q.Where[1].lString != "w2" {
			t.Errorf("Expected w2 as second element in 'where' clause but got '%v': %s\n%v", q.Where[1].lString, queryStr, q)
		}
		if q.Where[1].Operation != StringEq {
			t.Errorf("Expected StringEq operation in second 'where' condition but got '%v': %s\n%v", q.Where[0].Operation, queryStr, q)
		}
		if q.Where[1].rString != "free beer" {
			t.Errorf("Expected 'free beer' as string argument in second 'where' condition but got '%v': %s\n%v", q.Where[0].rString, queryStr, q)
		}

		// 'group by' clause
		if len(q.GroupBy) != 2 {
			t.Errorf("Expected two elements in 'group by' clause but got '%v': %s\n%v", q.GroupBy, queryStr, q)
		}
		if q.GroupBy[0] != "g1" {
			t.Errorf("Expected 'g1' as first element in 'group by' clause but got '%v': %s\n%v", q.GroupBy[0], queryStr, q)
		}
		if q.GroupBy[1] != "g2" {
			t.Errorf("Expected 'g2' as second element in 'group by' clause but got '%v': %s\n%v", q.GroupBy[1], queryStr, q)
		}
		if q.GroupKey != "g1,g2" {
			t.Errorf("Expected 'g1,g2' as group key in 'group by' clause but got '%v': %s\n%v", q.GroupKey, queryStr, q)
		}

		// 'order by' clause
		if q.OrderBy != "count(s3)" {
			t.Errorf("Expected 'count(s3)' as element in 'order by' clause but got '%v': %s\n%v", q.OrderBy, queryStr, q)
		}

		// 'interval' clause
		if q.Interval != time.Second*time.Duration(10) {
			t.Errorf("Expected '10s' as duration 'interval' clause but got '%v': %s\n%v", q.Interval, queryStr, q)
		}

		// 'limit' clause
		if q.Limit != 23 {
			t.Errorf("Expected '23' as limit in 'limit' clause but got '%v': %s\n%v", q.Limit, queryStr, q)
		}
	}
}
