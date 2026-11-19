package clients

import (
	"bytes"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/omode"
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

// TestApplyQueryRegexIgnoresInvert pins that a mapreduce query derives its
// line filter from its table and never inverts it: inverted, the filter would
// feed the other tables' lines (or only the empty lines) into the aggregation.
func TestApplyQueryRegexIgnoresInvert(t *testing.T) {
	t.Parallel()

	for _, query := range []*mapr.Query{nil, {Table: ""}, {Table: "."}, {Table: "*"}, {Table: "STATS"}} {
		args := config.Args{RegexStr: "user regex", RegexInvert: true}
		applyQueryRegex(&args, query)
		if want := maprRegexForQuery(query); args.RegexStr != want || args.RegexInvert {
			t.Errorf("applyQueryRegex(%v) = regex %q invert %v, want regex %q invert false",
				query, args.RegexStr, args.RegexInvert, want)
		}
	}
}

// TestWarnIgnoredInvert tells the user about a dropped --invert, and only then.
func TestWarnIgnoredInvert(t *testing.T) {
	t.Parallel()

	for _, invert := range []bool{false, true} {
		var out bytes.Buffer
		warnIgnoredInvert(&out, config.Args{RegexInvert: invert}, logging.NopLogger{})
		want := ""
		if invert {
			want = ignoredInvertWarning + "\n"
		}
		if out.String() != want {
			t.Errorf("warning with invert %v = %q, want %q", invert, out.String(), want)
		}
	}
}

// TestNormalizeInteractiveArgsIgnoresInvert covers the interactive query
// path, which builds its session spec from normalized args.
func TestNormalizeInteractiveArgsIgnoresInvert(t *testing.T) {
	t.Parallel()

	args, err := normalizeInteractiveArgs(config.Args{
		Mode:        omode.TailClient,
		QueryStr:    "from STATS select count(*)",
		RegexInvert: true,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeInteractiveArgs() error = %v", err)
	}
	if args.RegexStr != `\|MAPREDUCE:STATS\|` || args.RegexInvert {
		t.Fatalf("normalized args = regex %q invert %v, want the STATS filter, not inverted",
			args.RegexStr, args.RegexInvert)
	}
}
