package mapr

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

func TestParserFieldPlan(t *testing.T) {
	q, err := NewQuery(
		"select count($derived) from STATS where $goroutines > 10 "+
			"set $derived = md5sum(foo), $other = $derived group by $derived", logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("Unable to create query: %s", err.Error())
	}

	plan := q.ParserFieldPlan()
	if plan.AllFields {
		t.Fatalf("Expected query-specific field plan")
	}

	requiredFields := []string{"foo", "$goroutines"}
	for _, field := range requiredFields {
		if !plan.Needs(field) {
			t.Errorf("Expected field '%s' to be required", field)
		}
	}

	notRequiredFields := []string{"$derived", "$other", "$time"}
	for _, field := range notRequiredFields {
		if plan.Needs(field) {
			t.Errorf("Expected field '%s' to not be required", field)
		}
	}
}
