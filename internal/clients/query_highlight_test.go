package clients

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
)

// highlightCorpus are queries the highlighter must keep byte for byte:
// every clause, quoted strings with blanks, escapes and keywords, backticked
// field names, set conditions, unicode, invalid UTF-8, partial and broken
// queries.
var highlightCorpus = []string{
	"",
	"from STATS select count($line) group by $hostname",
	"from STATS select $hostname,max($goroutines) group by $hostname order by max($goroutines) interval 10",
	`select count(status) from WEB where status == "500 from where" and bytes > 100 group by host`,
	`select avg(bytes),last(path) from WEB where path ne "a \"quoted\" \\ path" group by path rorder avg(bytes) limit 5`,
	"select count(`select`),`from` from T group by `group`",
	`select md5 from T set $md5 = md5sum(user), $x = maskdigits($line) group by md5 outfile "/tmp/out file.csv" append`,
	"select count(größe) from ÜBER where größe >= 1.5e3 group by größe logformat generic",
	"select count(a) from T where a == \"\xff\xfe\" group by a",
	"select count(  from where ((( ,,, \"unclosed",
	"  SELECT Count(x)  FROM t\tGROUP BY x  ",
	"garbage before select x from y",
	"select `unclosed backtick",
}

// TestQueryHighlightKeepsText checks that the highlighted query equals the
// query (and the quoted one strconv.Quote of it) once the color sequences
// are stripped, with and without a base color.
func TestQueryHighlightKeepsText(t *testing.T) {
	theme := config.DefaultTermColors().Interactive
	under := string(color.FgCyan) + string(color.BgBlack)
	for _, base := range []string{"", under + string(color.AttrDim)} {
		h := queryHighlighter{theme: theme, base: base, under: under}
		for _, query := range highlightCorpus {
			if got := stripANSI(h.plain(query)); got != query {
				t.Errorf("base %q: plain(%q) stripped = %q", base, query, got)
			}
			if got, want := stripANSI(h.quoted(query)), strconv.Quote(query); got != want {
				t.Errorf("base %q: quoted(%q) stripped = %q, want %q", base, query, got, want)
			}
		}
	}
}

// TestQueryHighlightPaintsTokens checks the style of each kind of token.
func TestQueryHighlightPaintsTokens(t *testing.T) {
	s := testInteractiveStyle()
	theme := s.theme
	// == is numeric (a quoted value after it is a word the parser rejects,
	// left unpainted); eq compares strings.
	got := s.query(`select count(status) from WEB where status eq "500" and bytes > 100 group by $hostname`)
	for _, want := range []string{
		s.paint(theme.CandidateKeyword, "select"),
		s.paint(theme.CandidateFunction, "count"),
		s.paint(theme.CandidateField, "status"),
		s.paint(theme.CandidateTable, "WEB"),
		s.paint(theme.CandidateOperator, "eq"),
		s.paint(theme.CandidateValue, `"500"`),
		s.paint(theme.CandidateValue, "100"),
		s.paint(theme.CandidateVariable, "$hostname"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("highlighted query %q lacks %q", got, want)
		}
	}
	if rejected := s.query(`where a == "x"`); !strings.Contains(rejected, ` "x"`) {
		t.Errorf("a value the parser rejects is painted: %q", rejected)
	}
	var off *interactiveStyle
	if got := off.query("select x"); got != "select x" {
		t.Errorf("query without colors = %q", got)
	}
	if got := off.quotedQuery(`a "b"`); got != `"a \"b\""` {
		t.Errorf("quoted query without colors = %q", got)
	}
}

// TestQueryHighlightBase checks that the base color surrounds the query,
// is set again after every token and reset at the end, and that a token
// keeps the base colors but not its attribute (no dimmed keywords).
func TestQueryHighlightBase(t *testing.T) {
	theme := config.DefaultTermColors().Interactive
	under := string(color.FgCyan) + string(color.BgBlack)
	base := under + string(color.AttrDim)
	got := queryHighlighter{theme: theme, base: base, under: under}.plain("select x")
	want := base + string(color.AttrReset) + under + string(theme.CandidateKeyword.Fg) +
		string(theme.CandidateKeyword.Attr) + "select" + string(color.AttrReset) + string(color.FgDefault) + base + " "
	if !strings.HasPrefix(got, want) {
		t.Fatalf("highlighted = %q, want prefix %q", got, want)
	}
	if end := string(color.AttrReset) + string(color.BgDefault) + string(color.FgDefault); !strings.HasSuffix(got, end) {
		t.Fatalf("highlighted = %q, want suffix %q", got, end)
	}
}

// TestReloadAppliedMessage checks the answer of an applied :reload: the
// query it applied is echoed, highlighted with colors.
func TestReloadAppliedMessage(t *testing.T) {
	if got := reloadAppliedMessage(nil, ""); got != "reload applied successfully" {
		t.Fatalf("without a query = %q", got)
	}
	if got := reloadAppliedMessage(nil, "select x"); got != "reload applied successfully\nquery: select x" {
		t.Fatalf("with a query = %q", got)
	}
	s := testInteractiveStyle()
	got := reloadAppliedMessage(s, "select x")
	if stripANSI(got) != "reload applied successfully\nquery: select x" || !strings.Contains(got, s.query("select x")) {
		t.Fatalf("colored = %q", got)
	}
}

// FuzzQueryHighlightKeepsText checks for any query that the highlighted
// text equals the query, and the quoted one strconv.Quote of it, once the
// color sequences are stripped. Queries containing ESC are skipped, as
// stripANSI would strip their own sequences too.
func FuzzQueryHighlightKeepsText(f *testing.F) {
	for _, query := range highlightCorpus {
		f.Add(query)
	}
	f.Add("select count(`a b`) from T where x eq \"\u2028\" group by x")
	h := queryHighlighter{theme: config.DefaultTermColors().Interactive}
	f.Fuzz(func(t *testing.T, query string) {
		if strings.ContainsRune(query, 0x1b) {
			t.Skip()
		}
		if got := stripANSI(h.plain(query)); got != query {
			t.Fatalf("plain(%q) stripped = %q", query, got)
		}
		if got, want := stripANSI(h.quoted(query)), strconv.Quote(query); got != want {
			t.Fatalf("quoted(%q) stripped = %q, want %q", query, got, want)
		}
	})
}
