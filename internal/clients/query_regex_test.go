package clients

import (
	"testing"

	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/regex"
)

// TestMaprRegexForQueryStaysALiteral pins the shape of the MapReduce line
// filter: it escapes the surrounding pipes, so it must be recognized as a
// literal and matched with a plain substring search instead of the regexp
// engine, which is the hot path of every dmap run.
func TestMaprRegexForQueryStaysALiteral(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		query       *mapr.Query
		wantRegex   string
		wantMatch   []string
		wantNoMatch []string
	}{
		{
			name:        "named table",
			query:       &mapr.Query{Table: "STATS"},
			wantRegex:   `\|MAPREDUCE:STATS\|`,
			wantMatch:   []string{"INFO|1|MAPREDUCE:STATS|a=b"},
			wantNoMatch: []string{"INFO|1|MAPREDUCE:OTHER|a=b", "INFO|1|MAPREDUCEXSTATSXa=b"},
		},
		{
			name:        "any table",
			query:       &mapr.Query{Table: "*"},
			wantRegex:   `\|MAPREDUCE:\|`,
			wantMatch:   []string{"INFO|1|MAPREDUCE:|a=b"},
			wantNoMatch: []string{"INFO|1|MAPREDUCE:STATS|a=b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotRegex := maprRegexForQuery(tt.query)
			if gotRegex != tt.wantRegex {
				t.Fatalf("maprRegexForQuery() = %q, want %q", gotRegex, tt.wantRegex)
			}

			re, err := regex.New(gotRegex, regex.Default)
			if err != nil {
				t.Fatalf("regex.New(%q) error = %v", gotRegex, err)
			}
			if !re.IsLiteral() {
				t.Errorf("regex %q must use literal matching", gotRegex)
			}
			for _, line := range tt.wantMatch {
				if !re.MatchString(line) {
					t.Errorf("regex %q should match %q", gotRegex, line)
				}
			}
			for _, line := range tt.wantNoMatch {
				if re.MatchString(line) {
					t.Errorf("regex %q should not match %q", gotRegex, line)
				}
			}
		})
	}
}

// TestMaprRegexForQueryWithoutTable keeps the no-op pattern for queries which
// do not filter by table at all.
func TestMaprRegexForQueryWithoutTable(t *testing.T) {
	t.Parallel()

	for _, query := range []*mapr.Query{nil, {Table: ""}, {Table: "."}} {
		if got := maprRegexForQuery(query); got != "." {
			t.Errorf("maprRegexForQuery(%v) = %q, want %q", query, got, ".")
		}
	}
}
