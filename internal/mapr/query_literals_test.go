package mapr

import (
	"context"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

// A bareword number of a string operation is a literal, as documented
// ("where $a eq 1 and $b ne 2"); it used to be looked up as a field.
func TestWhereStringOperationNumericLiteral(t *testing.T) {
	q, err := NewQuery("select count($line) from STATS where $a eq 1 and $b ne 2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !q.WhereClause(map[string]string{"$a": "1", "$b": "3"}) {
		t.Error("expected $a=1,$b=3 to match")
	}
	if q.WhereClause(map[string]string{"$a": "1", "$b": "2"}) {
		t.Error("expected $b=2 not to match")
	}

	// Backticks still make a number a field name, and words stay fields.
	q, err = NewQuery("select count($line) from STATS where $a eq `1` and $b eq inf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !q.WhereClause(map[string]string{"$a": "x", "1": "x", "$b": "y", "inf": "y"}) {
		t.Error("expected backticked number and word to be read as fields")
	}
}

func TestQueryRejectsInvalidClauses(t *testing.T) {
	for _, query := range []string{
		"select count($line) from STATS limit -5",
		"select count($line) from STATS interval -5",
		"select count($line) from STATS interval 99999999999",
		"select count(foo)bar from STATS",
	} {
		if _, err := NewQuery(query, nil); err == nil {
			t.Errorf("expected query to be rejected: %s", query)
		}
	}
	for _, query := range []string{
		"select count($line) from STATS limit 0",
		"select count($line) from STATS interval 3600 limit 5",
	} {
		if _, err := NewQuery(query, nil); err != nil {
			t.Errorf("expected query to parse: %s: %v", query, err)
		}
	}
}

// Log content containing the aggregate delimiter must neither cut its value
// short nor be parsed as further fields of the message.
func TestSerializeNeutralisesDelimiter(t *testing.T) {
	set := NewAggregateSet()
	if err := set.Aggregate("last(msg)", Last, "evil∥count(host)≔777", false); err != nil {
		t.Fatal(err)
	}
	if err := set.Aggregate("count(host)", Count, "x", false); err != nil {
		t.Fatal(err)
	}
	set.Samples = 1

	ch := make(chan string, 1)
	if !set.Serialize(context.Background(), "a∥b", ch, nil) {
		t.Fatal("serialize did not send")
	}
	got := <-ch
	// groupKey, samples, two fields and the empty part after the last delimiter.
	if parts := strings.Split(got, protocol.AggregateDelimiter); len(parts) != 5 {
		t.Fatalf("expected 5 parts, got %d: %q", len(parts), got)
	}
	if !strings.HasPrefix(got, "a||b"+protocol.AggregateDelimiter+"1"+protocol.AggregateDelimiter) {
		t.Errorf("unexpected group key and samples: %q", got)
	}
	if !strings.Contains(got, "last(msg)≔evil||count(host)≔777"+protocol.AggregateDelimiter) {
		t.Errorf("unexpected last() value: %q", got)
	}
}

func TestAggregateRejectsNonFiniteNumbers(t *testing.T) {
	set := NewAggregateSet()
	for _, value := range []string{"NaN", "nan", "Inf", "-infinity"} {
		for _, op := range []AggregateOperation{Sum, Min, Max, Avg} {
			if err := set.Aggregate("f", op, value, false); err == nil {
				t.Errorf("expected %q to be rejected for operation %v", value, op)
			}
		}
	}
	if len(set.FValues) != 0 {
		t.Errorf("expected no value to be stored, got %v", set.FValues)
	}
}

// A repeated last() value keeps the stored copy instead of cloning again.
func TestAggregateLastRepeatedValueDoesNotAllocate(t *testing.T) {
	set := NewAggregateSet()
	value := "hostname.example.org"
	if err := set.Aggregate("host", Last, value, false); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		_ = set.Aggregate("host", Last, value, false)
	})
	if allocs != 0 {
		t.Errorf("expected no allocation for a repeated value, got %v", allocs)
	}
	if err := set.Aggregate("host", Last, "other", false); err != nil {
		t.Fatal(err)
	}
	if set.SValues["host"] != "other" {
		t.Errorf("expected changed value to be stored, got %q", set.SValues["host"])
	}
}
