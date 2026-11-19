package mapr

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// TestNewQueryRejectsStrayWords checks that a word the parser would drop or
// misread is rejected with an error naming it: a word after the one value
// of from, order, rorder, interval, limit and logformat (the parser used to
// ignore it, so "limit 10 gro" was applied as "limit 10"), a word after the
// path of outfile, a value missing after interval, and a bare "by" in a
// select or group list (a misspelled clause keyword such as "gropu by x"
// used to select the fields gropu, by and x), and a first word that is no
// bare clause keyword (a quoted "select" used to start the select clause).
func TestNewQueryRejectsStrayWords(t *testing.T) {
	tests := []struct {
		query string
		want  string // a part of the error message
		// incomplete: AnalyzePartial reads the query as still being typed
		// (a number is expected), so it has no rejected token to mark.
		incomplete bool
	}{
		{"from STATS select count(x) order by count(x) gro", `"gro"`, false},
		{"from STATS select count(x) rorder count(x) gro", `"gro"`, false},
		{"from STATS select count(x) interval 10 gro", `"gro"`, false},
		{"from STATS select count(x) limit 10 20", `"20"`, false},
		{"from STATS select count(x) logformat default gro", `"gro"`, false},
		{"from STATS gro select count(x)", `"gro"`, false},
		{`from STATS select count(x) outfile "a.csv" gro`, `"gro"`, false},
		{`from STATS select count(x) outfile append "a.csv" gro`, `"gro"`, false},
		{`from STATS select count(x) outfile "a.csv" "b.csv"`, `"b.csv"`, false},
		{"from STATS select count(x) interval", "'interval'", true},
		{"from STATS select count(x) gropu by x", `"by"`, false},
		{"from STATS select count(x) BY x", `"BY"`, false},
		{"from STATS select count(x) group by x gropu by y", `"by"`, false},
		{"from STATS select count(x) group by by", `"by"`, false},
		{"from STATS select count(x) group x by", `"by"`, false},
		{"from STATS select count(x) order by count(x) asc", "ascending", false},
		{"from STATS select count(x) order by count(x) desc desc", `unexpected word "desc"`, false},
		{"from STATS select count(x) order by count(x) desc gro", `"gro"`, false},
		{"select a order by a desc limt 10", `"limt"`, false},
		{"select a,b order by a desc, b desc", `"b"`, false},
		{"select a,b order by a,b", `"b"`, false},
		{"select a order by a reverse", `"reverse"`, false},
		{"from STATS select count(x) rorder by count(x) desc", "descending", false},
		{"from STATS select count(x) rorder by count(x) asc gro", `"gro"`, false},
		{"from STATS select count(x) rorder by count(x) ASC asc", `unexpected word "asc"`, false},
		{`"select" foo`, `unexpected word "select"`, false},
		{`"SeleCt"0`, `unexpected word "SeleCt"`, false},
		{`"from" STATS select count(x)`, `unexpected word "from"`, false},
		{"`select` a", "unexpected word \"`select`\"", false},
		{"gro select a", `unexpected word "gro"`, false},
	}
	for _, tt := range tests {
		q, err := NewQuery(tt.query, logging.NopLogger{})
		if err == nil {
			t.Errorf("NewQuery(%q) = %v, want an error", tt.query, q)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("NewQuery(%q) error %q does not name %s", tt.query, err, tt.want)
		}
		if tt.incomplete {
			continue
		}
		p := AnalyzePartial(tt.query + " ")
		if !slices.ContainsFunc(p.Tokens, func(tok PartialToken) bool {
			return tok.Kind == TokenUnknown
		}) {
			t.Errorf("AnalyzePartial(%q): no unknown token in %v", tt.query, p.Tokens)
		}
	}
}

// TestNewQueryAcceptsWordLists checks the lists that stay valid: select,
// group, where and set lists separated by blanks instead of commas, a field
// named by when it is escaped with backticks, and the direction word after
// the field of order (desc) and rorder (asc), the direction each sorts in
// anyway (existing queries write order ... desc). A stray word in a
// select or group list is a field, as the language cannot tell it from one.
func TestNewQueryAcceptsWordLists(t *testing.T) {
	tests := []struct {
		query   string
		selects int
		groupBy []string
	}{
		{"from STATS select count(x) gro", 2, []string{"x"}},
		{"select a b c group by a b", 3, []string{"a", "b"}},
		{"select a where a eq 1 b eq 2 set $c = 1 $d = 2", 1, []string{"a"}},
		{"select `by`,count(x) group by `by`", 2, []string{"by"}},
		{"select `BY` group `by` x", 1, []string{"by", "x"}},
		{"select a group BY a order BY a", 1, []string{"a"}},
		{`select a outfile append "a.csv"`, 1, []string{"a"}},
		{"select a outfile append", 1, []string{"a"}},
		{"select a order by a desc", 1, []string{"a"}},
		{"select a order a DESC limit 3", 1, []string{"a"}},
		{"select a rorder by a asc", 1, []string{"a"}},
		{"select a rorder a ASC limit 3", 1, []string{"a"}},
		{"select `desc` order by `desc`", 1, []string{"desc"}},
	}
	for _, tt := range tests {
		q, err := NewQuery(tt.query, logging.NopLogger{})
		if err != nil {
			t.Errorf("NewQuery(%q): %v", tt.query, err)
			continue
		}
		if len(q.Select) != tt.selects || !slices.Equal(q.GroupBy, tt.groupBy) {
			t.Errorf("NewQuery(%q): %d select fields, group by %v; want %d, %v",
				tt.query, len(q.Select), q.GroupBy, tt.selects, tt.groupBy)
		}
		for _, tok := range AnalyzePartial(tt.query + " ").Tokens {
			if tok.Kind == TokenUnknown {
				t.Errorf("AnalyzePartial(%q): unknown token %q", tt.query, tok.Text)
			}
		}
	}
}

// TestOrderDescKeepsDirection checks that the desc after the field of an
// order clause changes nothing: the query sorts descending as without it.
func TestOrderDescKeepsDirection(t *testing.T) {
	plain, err := NewQuery("select a order by a", logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := NewQuery("select a order by a desc", logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if desc.OrderBy != plain.OrderBy || desc.ReverseOrder || plain.ReverseOrder {
		t.Fatalf("order by a desc: OrderBy %q reverse %v, order by a: %q %v",
			desc.OrderBy, desc.ReverseOrder, plain.OrderBy, plain.ReverseOrder)
	}
	p := AnalyzePartial("select a order by a desc ")
	if last := p.Tokens[len(p.Tokens)-1]; last.Text != "desc" || last.Kind != TokenKeyword {
		t.Fatalf("AnalyzePartial: last token %+v, want desc as keyword", last)
	}
}

// strayWordPattern finds the word an "unexpected word" error names.
var strayWordPattern = regexp.MustCompile(`unexpected word ("(?:[^"\\]|\\.)*")`)

// checkStrayWordAgreement checks that AnalyzeQuery (AnalyzePartial on the
// whole query) agrees with NewQuery on stray words: a query the parser
// accepts has no unknown token, and the word an "unexpected word" error
// names is an unknown token (a quoted word with its quotes, also an
// unterminated one, a backticked one with its backticks). FuzzNewQuery
// checks both halves too.
func checkStrayWordAgreement(t *testing.T, query string) {
	t.Helper()
	_, err := NewQuery(query, logging.NopLogger{})
	var unknown []string
	for _, tok := range AnalyzeQuery(query).Tokens {
		if tok.Kind == TokenUnknown {
			unknown = append(unknown, tok.Text)
		}
	}
	if err == nil {
		if len(unknown) > 0 {
			t.Errorf("NewQuery(%q) accepts it, AnalyzeQuery marks %q unknown", query, unknown)
		}
		return
	}
	m := strayWordPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return
	}
	word, uerr := strconv.Unquote(m[1])
	if uerr != nil {
		t.Fatalf("NewQuery(%q): unquote %s: %v", query, m[1], uerr)
	}
	if !slices.ContainsFunc(unknown, func(text string) bool {
		return text == word || text == `"`+word+`"` || text == `"`+word ||
			text == "`"+word+"`"
	}) {
		t.Errorf("NewQuery(%q) rejects %q, AnalyzeQuery marks only %q unknown",
			query, word, unknown)
	}
}

// TestStrayWordsAgreeWithAnalyzePartial runs checkStrayWordAgreement on
// every clause followed by stray words, direction words and a misspelled
// clause keyword, and on the valid queries.
func TestStrayWordsAgreeWithAnalyzePartial(t *testing.T) {
	clauses := []string{"", "from STATS", "order by a", "rorder a", "interval 5",
		"limit 3", "logformat csv", `outfile "o.csv"`, `outfile append "o.csv"`,
		"group by a", "where a eq 1", "set $b = 1"}
	suffixes := []string{"", "gro", "desc", "asc", "DESC gro", "asc limt 10",
		"desc, b desc", "gropu by a", "`by`", `"q"`, "by"}
	queries := slices.Clone(validQueries)
	for _, clause := range clauses {
		for _, suffix := range suffixes {
			queries = append(queries, strings.TrimSpace("select a,b "+clause+" "+suffix))
		}
	}
	for _, query := range queries {
		checkStrayWordAgreement(t, query)
	}
}
