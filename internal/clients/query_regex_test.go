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
			// from * reads every MAPREDUCE line, whatever its table, as
			// doc/querylanguage.md says; it used to match only the lines
			// of the table without a name.
			name:      "any table",
			query:     &mapr.Query{Table: "*"},
			wantRegex: `\|MAPREDUCE:`,
			wantMatch: []string{"INFO|1|MAPREDUCE:|a=b", "INFO|1|MAPREDUCE:STATS|a=b",
				"INFO|1|MAPREDUCE:stats|a=b"},
			wantNoMatch: []string{"INFO|1|a=b", "INFO|1|XMAPREDUCE:STATS|a=b", "INFO|1|MAPREDUCE|a=b"},
		},
		{
			// A table name is a literal: its regex metacharacters match
			// themselves only.
			name:        "table with a plus",
			query:       &mapr.Query{Table: "A+B"},
			wantRegex:   `\|MAPREDUCE:A\+B\|`,
			wantMatch:   []string{"INFO|1|MAPREDUCE:A+B|a=b"},
			wantNoMatch: []string{"INFO|1|MAPREDUCE:AAB|a=b", "INFO|1|MAPREDUCE:AB|a=b"},
		},
		{
			name:        "table with a dot",
			query:       &mapr.Query{Table: "A.B"},
			wantRegex:   `\|MAPREDUCE:A\.B\|`,
			wantMatch:   []string{"INFO|1|MAPREDUCE:A.B|a=b"},
			wantNoMatch: []string{"INFO|1|MAPREDUCE:AXB|a=b"},
		},
		{
			name:        "table of metacharacters",
			query:       &mapr.Query{Table: `A(B)[C]{2}^$?*\D`},
			wantRegex:   `\|MAPREDUCE:A\(B\)\[C\]\{2\}\^\$\?\*\\D\|`,
			wantMatch:   []string{`INFO|1|MAPREDUCE:A(B)[C]{2}^$?*\D|a=b`},
			wantNoMatch: []string{"INFO|1|MAPREDUCE:ABCC|a=b", `INFO|1|MAPREDUCE:A(B)[C]{2}^$?*5|a=b`},
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
	})
	if err != nil {
		t.Fatalf("normalizeInteractiveArgs() error = %v", err)
	}
	if args.RegexStr != `\|MAPREDUCE:STATS\|` || args.RegexInvert {
		t.Fatalf("normalized args = regex %q invert %v, want the STATS filter, not inverted",
			args.RegexStr, args.RegexInvert)
	}
}

// TestParsedQueryTableFilter runs the table filter of parsed queries over
// lines of several tables: a from clause selects exactly the lines of its
// table, from * those of every table and no from clause every line.
func TestParsedQueryTableFilter(t *testing.T) {
	t.Parallel()

	lines := []string{
		"INFO|1|MAPREDUCE:STATS|a=b",
		"INFO|1|MAPREDUCE:A+B|a=b",
		"INFO|1|MAPREDUCE:AAB|a=b",
		"INFO|1|MAPREDUCE:|a=b",
		"INFO|1|no table",
	}
	tests := []struct {
		query string
		want  []bool
	}{
		{"from STATS select count($line)", []bool{true, false, false, false, false}},
		{"from stats select count($line)", []bool{true, false, false, false, false}},
		{`from "A+B" select count($line)`, []bool{false, true, false, false, false}},
		{"from * select count($line)", []bool{true, true, true, true, false}},
		{"from . select count($line)", []bool{true, true, true, true, true}},
		{"select count($line)", []bool{true, true, true, true, true}},
		{"from NONE select count($line)", []bool{false, false, false, false, false}},
	}
	for _, tt := range tests {
		query, err := mapr.NewQuery(tt.query, logging.NopLogger{})
		if err != nil {
			t.Fatalf("NewQuery(%q) error = %v", tt.query, err)
		}
		re, err := regex.New(maprRegexForQuery(query), regex.Default)
		if err != nil {
			t.Fatalf("%q: regex.New(%q) error = %v", tt.query, maprRegexForQuery(query), err)
		}
		for i, line := range lines {
			if got := re.MatchString(line); got != tt.want[i] {
				t.Errorf("%q (filter %q) matches %q = %v, want %v", tt.query, re, line, got, tt.want[i])
			}
		}
	}
}
