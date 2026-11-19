package clients

import (
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/mapr"
)

// quoteCatalog has fields named like clause keywords, with a blank and in
// backticks, which order by, select and group must write differently.
var quoteCatalog = catalogSnapshot{
	DefaultLogFormat: "default",
	Servers:          []string{"srv1"},
	Formats: map[string]catalogFormat{
		"default": {
			Tables: map[string]map[string][]string{
				"STATS": {"limit": nil, "foo bar": nil, "x": nil, "f(y)": nil},
			},
			Variables: map[string][]string{"$hostname": {"web1"}},
		},
	},
}

// completeOnly completes the word at the end of prefix and returns the
// query with the only candidate inserted; it fails unless there is exactly
// one.
func completeOnly(t *testing.T, prefix string) string {
	t.Helper()
	got := completeQueryWord(prefix, "", quoteCatalog)
	if len(got.candidates) != 1 {
		t.Fatalf("completeQueryWord(%q): candidates %q, want exactly one", prefix, got.candidates)
	}
	return prefix[:len(prefix)-len(got.word)] + got.candidates[0]
}

// TestCompleteQueryWrittenItemsParse completes a word at the end of each
// prefix, adds the rest of the query and checks that the parser reads the
// completed word as intended: select items in order by written as the
// tokenizer needs them (a keyword in backticks, a name with a blank in
// double quotes), and inside an open double quote only words that can be
// written there, with the quote closed.
func TestCompleteQueryWrittenItemsParse(t *testing.T) {
	const items = "from STATS select `limit`,\"foo bar\",count(x) "
	tests := []struct {
		prefix, rest string
		check        func(*mapr.Query) bool
	}{
		{items + "order by li", "", func(q *mapr.Query) bool { return q.OrderBy == "limit" }},
		{items + "order by `li", "", func(q *mapr.Query) bool { return q.OrderBy == "limit" }},
		{items + "order by \"li", "", func(q *mapr.Query) bool { return q.OrderBy == "limit" }},
		{items + "order by fo", "", func(q *mapr.Query) bool { return q.OrderBy == "foo bar" }},
		{items + "rorder by \"fo", "", func(q *mapr.Query) bool { return q.OrderBy == "foo bar" }},
		{items + "order by cou", "", func(q *mapr.Query) bool { return q.OrderBy == "count(x)" }},
		{"from STATS select \"by\" order by b", "", func(q *mapr.Query) bool { return q.OrderBy == "by" }},
		{"from STATS select \"by\" order \"b", "", func(q *mapr.Query) bool { return q.OrderBy == "by" }},
		{"from STATS select \"``x``\" order by `", "", func(q *mapr.Query) bool { return q.OrderBy == "`x`" }},
		{"select x from \"ST", "", func(q *mapr.Query) bool { return q.Table == "STATS" }},
		{"select x from STATS logformat \"cs", "", func(q *mapr.Query) bool { return q.LogFormat == "csv" }},
		{"from STATS select \"foo b", "", func(q *mapr.Query) bool { return q.Select[0].Field == "foo bar" }},
		{"from STATS select \"li", "", func(q *mapr.Query) bool { return q.Select[0].Field == "limit" }},
		{"from STATS select \"f(", "", func(q *mapr.Query) bool {
			return q.Select[0].Field == "f(y)" && q.Select[0].Operation == mapr.Last
		}},
		{"from STATS select x group by \"f(", "", func(q *mapr.Query) bool { return q.GroupBy[0] == "f(y)" }},
		{"from STATS select x group by \"foo", "", func(q *mapr.Query) bool { return q.GroupBy[0] == "foo bar" }},
		{"from STATS select x where $hostname eq \"w", "", func(q *mapr.Query) bool { return len(q.Where) == 1 }},
	}
	for _, tc := range tests {
		query := completeOnly(t, tc.prefix) + tc.rest
		q, err := mapr.NewQuery(query, nil)
		if err != nil {
			t.Fatalf("%q completed to %q: %v", tc.prefix, query, err)
		}
		if !tc.check(q) {
			t.Fatalf("%q completed to %q: parsed as %+v", tc.prefix, query, q)
		}
	}
}

// TestCompleteQueryNothingUnwritableInQuote checks the positions where an
// open double quote turns every word into something else (a string literal,
// a rejected quoted $variable or operator): nothing is offered there.
func TestCompleteQueryNothingUnwritableInQuote(t *testing.T) {
	for _, prefix := range []string{
		"select x from STATS set \"",
		"select x from STATS set \"$",
		"select x from STATS where x \"",
		"select x from STATS where \"",
		"select x from STATS set $a = \"",
		"select x from STATS set $a = \"md",
		"select x from STATS where x eq 1 \"",
		"select x from STATS order \"b",
		"select x from STATS outfile \"",
		"select \"cou",
	} {
		if got := completeQueryWord(prefix, "", quoteCatalog); len(got.candidates) != 0 {
			t.Errorf("completeQueryWord(%q): candidates %q, want none", prefix, got.candidates)
		}
	}
}

// TestQuerySelectItemWords checks how select items are written in order by:
// a name the tokenizer splits double-quoted, a keyword (and by, the lead
// word) in backticks, one wrapped in backticks in another pair, and one with
// a double quote, which nothing can write, left out.
func TestQuerySelectItemWords(t *testing.T) {
	items := []string{"a", "count(x)", "limit", "BY", "foo bar", "a,b", "`x`", `q"x`}
	tests := []struct {
		quote string
		want  []string
	}{
		{"", []string{"a ", "count(x) ", "`limit` ", "`BY` ", `"foo bar" `, `"a,b" `, "``x`` "}},
		{`"`, []string{`a" `, `count(x)" `, `limit" `, "`BY`\" ", `foo bar" `, `a,b" `, "``x``\" "}},
		{"`", []string{"a` ", "count(x)` ", "limit` ", "BY` ", "`x`` "}},
	}
	for _, tc := range tests {
		got := querySelectItemWords(items, tc.quote)
		if !slices.Equal(got, tc.want) {
			t.Errorf("quote %q: words %q, want %q", tc.quote, got, tc.want)
		}
	}
}

// TestQueryFieldWordsBy checks that a field named by is written in
// backticks, since group would drop a bare one as its optional by, and
// that the completed group by query parses with that field.
func TestQueryFieldWordsBy(t *testing.T) {
	if got := queryFieldWords([]string{"by", "By"}, false); !slices.Equal(got, []string{"`by`", "`By`"}) {
		t.Fatalf("words %q", got)
	}
	catalog := catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {Tables: map[string]map[string][]string{
			"STATS": {"by": nil}}}}}
	prefix := "from STATS select x group b"
	got := completeQueryWord(prefix, "", catalog)
	if !slices.Equal(got.candidates, []string{"`by` ", "by "}) {
		t.Fatalf("candidates %q", got.candidates)
	}
	q, err := mapr.NewQuery(prefix[:len(prefix)-1]+"`by` ", nil)
	if err != nil || !slices.Equal(q.GroupBy, []string{"by"}) {
		t.Fatalf("group `by`: %v, %v", q, err)
	}
}

// TestCompleteQueryBackticksAndNBSP covers tables inside backticks (closed
// by the backtick), sample values wrapped in backticks (another pair, as
// the parser strips one from a quoted token) and a field with a
// non-breaking space, which the tokenizer splits, so it is not offered
// bare. Each completed query is parsed back.
func TestCompleteQueryBackticksAndNBSP(t *testing.T) {
	catalog := catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {Tables: map[string]map[string][]string{
			"STATS": {"v": {"`w`", `a"b`}, "nb sp": nil},
			"by":    {"x": nil},
		}}}}
	tests := []struct {
		prefix string
		check  func(*mapr.Query) bool
	}{
		{"select x from `ST", func(q *mapr.Query) bool { return q.Table == "STATS" }},
		{"select x from `b", func(q *mapr.Query) bool { return q.Table == "BY" }},
		{"select x from b", func(q *mapr.Query) bool { return q.Table == "BY" }},
		{"from STATS select v where v eq ``", hasStringValue("`w`")},
		{"from STATS select v where v eq \"`", hasStringValue("`w`")},
	}
	for _, tc := range tests {
		got := completeQueryWord(tc.prefix, "", catalog)
		if len(got.candidates) != 1 {
			t.Fatalf("%q: candidates %q, want one", tc.prefix, got.candidates)
		}
		query := tc.prefix[:len(tc.prefix)-len(got.word)] + got.candidates[0]
		q, err := mapr.NewQuery(query, nil)
		if err != nil || !tc.check(q) {
			t.Fatalf("%q completed to %q: %+v, %v", tc.prefix, query, q, err)
		}
		if strings.Contains(tc.prefix, "where") && !strings.Contains(query, "\"``w``\" ") {
			t.Fatalf("%q completed to %q, want the value \"``w``\"", tc.prefix, query)
		}
	}
	if got := completeQueryWord("from STATS select nb", "", catalog); len(got.candidates) != 0 {
		t.Fatalf("field with a non-breaking space offered bare: %q", got.candidates)
	}
	if got := completeQueryWord("from STATS select \"nb", "", catalog); !slices.Equal(got.candidates, []string{"nb sp\" "}) {
		t.Fatalf("quoted field with a non-breaking space: %q", got.candidates)
	}
}

// hasStringValue checks that the first where condition compares with the
// string value (its String form lists rString and rType).
func hasStringValue(value string) func(*mapr.Query) bool {
	return func(q *mapr.Query) bool {
		return len(q.Where) == 1 &&
			strings.Contains(q.Where[0].String(), "rString:"+value+",") &&
			strings.Contains(q.Where[0].String(), "rType:String")
	}
}
