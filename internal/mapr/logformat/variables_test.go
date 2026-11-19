package logformat

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// TestPlanVariableWarnings locks the plan-time unknown-$-variable diagnostic.
// The warning exists to surface the silent footgun where a mistyped or wrongly
// $-prefixed field (e.g. "$service" instead of the bareword "service", or
// "$time" without a "from"/logformat clause) collapses an aggregation into a
// single empty group with zero other diagnostics.
func TestPlanVariableWarnings(t *testing.T) {
	tests := []struct {
		name string
		// query is parsed and its parser resolved via EffectiveLogFormat so the
		// test exercises the same selection rule the client uses.
		query string
		// wantVars lists the $-variables that must be flagged (each appears in
		// exactly one warning line).
		wantVars []string
		// unwantVars lists names that must NOT appear in any warning.
		unwantVars []string
	}{
		{
			name:       "ys0 footgun: $-prefixed dynamic field with from clause",
			query:      "from STATS select $service,sum($bytes) group by $service",
			wantVars:   []string{"$service", "$bytes"},
			unwantVars: []string{"service", "bytes"},
		},
		{
			name:       "generic parser without from warns for default-format $time",
			query:      "select $time,$line",
			wantVars:   []string{"$time"},
			unwantVars: []string{"$line"},
		},
		{
			name:       "valid default-format variables do not warn",
			query:      "from STATS select $hostname,max($goroutines),$time group by $hostname",
			wantVars:   nil,
			unwantVars: []string{"$hostname", "$goroutines", "$time"},
		},
		{
			name:       "barewords never warn",
			query:      "from STATS select service,sum(bytes) group by service",
			wantVars:   nil,
			unwantVars: []string{"service", "bytes", "$service", "$bytes"},
		},
		{
			name:       "built-in $empty does not warn",
			query:      "from STATS select $empty,count($line) group by $empty",
			wantVars:   nil,
			unwantVars: []string{"$empty", "$line"},
		},
		{
			name:       "set-defined variable does not warn",
			query:      "from STATS select $masked group by $masked set $masked = maskdigits($line)",
			wantVars:   nil,
			unwantVars: []string{"$masked"},
		},
		{
			name:       "unknown variable referenced only in where clause warns",
			query:      "from STATS select $line where $bogus eq foo",
			wantVars:   []string{"$bogus"},
			unwantVars: []string{"$line"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query, err := mapr.NewQuery(tc.query, logging.NopLogger{})
			if err != nil {
				t.Fatalf("NewQuery(%q) failed: %v", tc.query, err)
			}
			logFormat := query.EffectiveLogFormat("")
			warnings := PlanVariableWarnings(query, logFormat)

			joined := strings.Join(warnings, "\n")
			for _, want := range tc.wantVars {
				if countVarWarnings(warnings, want) != 1 {
					t.Errorf("expected exactly one warning for %q, got warnings:\n%s",
						want, joined)
				}
			}
			for _, unwant := range tc.unwantVars {
				if countVarWarnings(warnings, unwant) != 0 {
					t.Errorf("did not expect a warning for %q, got warnings:\n%s",
						unwant, joined)
				}
			}
		})
	}
}

// TestPlanVariableWarningsText locks the exact warning wording so downstream
// tooling and users can rely on it.
func TestPlanVariableWarningsText(t *testing.T) {
	query, err := mapr.NewQuery("from STATS select $service group by $service", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery failed: %v", err)
	}
	warnings := PlanVariableWarnings(query, query.EffectiveLogFormat(""))
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning (deduped), got %d: %v", len(warnings), warnings)
	}
	want := `warning: $service is not a known variable for log format "default"; did you mean bareword service?`
	if warnings[0] != want {
		t.Errorf("warning text mismatch:\n got: %s\nwant: %s", warnings[0], want)
	}
}

// TestPlanVariableWarningsNonEnumerable ensures that log formats whose variable
// set cannot be determined statically produce no warnings, avoiding false
// positives that would train users to ignore the diagnostic.
func TestPlanVariableWarningsNonEnumerable(t *testing.T) {
	query, err := mapr.NewQuery("from STATS select $whatever group by $whatever", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery failed: %v", err)
	}
	for _, format := range []string{"mimecast", "custom1", "custom2", "doesnotexist"} {
		if warnings := PlanVariableWarnings(query, format); len(warnings) != 0 {
			t.Errorf("expected no warnings for non-enumerable format %q, got: %v",
				format, warnings)
		}
	}
}

// countVarWarnings counts warnings whose SUBJECT is the given variable, i.e.
// "warning: <variable> is not a known variable ...". It deliberately matches
// only the subject and not the "did you mean bareword X" suggestion, so that a
// bareword appearing in a suggestion is not mistaken for a warning about that
// bareword. The trailing " is not" also prevents "$service" from matching a
// hypothetical "$services" subject.
func countVarWarnings(warnings []string, variable string) int {
	subject := "warning: " + variable + " is not a known variable"
	count := 0
	for _, w := range warnings {
		if strings.HasPrefix(w, subject) {
			count++
		}
	}
	return count
}
