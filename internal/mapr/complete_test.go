package mapr

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr/funcs"
)

// scanCorpus holds queries, valid or not, on which scanQuery must split
// exactly as tokenize.
var scanCorpus = []string{
	"",
	" \t\n ",
	",,,",
	"select foo from bar",
	`select "free beer" from stats`,
	"select `from`,count(*) from stats",
	"\x00,\xff\"unterminated",
	`a"b"c`,
	`""`,
	`"`,
	`a "`,
	`a ""b`,
	`"x" "y"`,
	"select foo\u0085bar baz",
	"select foo,\vbar\f",
	"select `count($foo)` group by `count($foo)`",
	`select count($line) where $a eq "x, y" and $b == 1 outfile append "out.csv"`,
	"where a eq \"open quote",
	"ünïcödé,ẞ \"ü\" `",
}

// validQueries are complete queries the parser accepts; AnalyzePartial must
// agree with the parsed query on them.
var validQueries = []string{
	"select foo",
	"from STATS select $hostname,max($goroutines),lifetimeConnections group by $hostname",
	"select count($line),sum(bytes),avg(bytes) from stats where $a eq 1 and bytes >= 10 group $a order by sum(bytes) limit 10 interval 3",
	"select `count($foo)` group by `count($foo)`",
	"select foo,count(foo) rorder count(foo)",
	`select foo,count(foo) where foo contains "x y" foo ne "" outfile append "out.csv"`,
	"select $a set $a = md5sum(maskdigits($b)), $c = 42, $d = \"s\" , $e = `count(x)`",
	"select foo logformat csv",
	"select foo logformat generickv from bar",
	"SELECT foo FROM bar WHERE foo EQ 1 GROUP BY foo",
	"select foo where 3 < foo, `1` => 2, y lacks \"x\" and y ncontains z",
	"select foo from . outfile \"r.csv\"",
	"select `select`,`by` group by `by`",
	"select foo where foo eq \"and\" and and eq 1",
	"select foo order by foo select foo,bar",
	"select foo order by foo desc limit 1",
}

func TestScanQueryMatchesTokenize(t *testing.T) {
	for _, query := range append(slices.Clone(scanCorpus), validQueries...) {
		checkScan(t, query)
	}
}

func FuzzScanQuery(f *testing.F) {
	for _, seed := range append(slices.Clone(scanCorpus), validQueries...) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		checkScan(t, query)
		checkPartialInvariants(t, AnalyzePartial(query))
		checkWholeQuery(t, query)
	})
}

// checkWholeQuery checks that AnalyzeQuery reads a query as AnalyzePartial
// reads it followed by a blank (where the blank does not join an open
// quote): the state after its last token, no word.
func checkWholeQuery(t *testing.T, query string) {
	t.Helper()
	whole := AnalyzeQuery(query)
	checkPartialInvariants(t, whole)
	if whole.Word != "" || whole.InQuote || whole.InBacktick || whole.InAggregation {
		t.Fatalf("AnalyzeQuery(%q): word %q in a quote, backticks or call", query, whole.Word)
	}
	if strings.Count(query, `"`)%2 != 0 {
		return
	}
	blank := AnalyzePartial(query + " ")
	if whole.Clause != blank.Clause || whole.Expect != blank.Expect || whole.Table != blank.Table ||
		whole.LogFormat != blank.LogFormat || !slices.Equal(whole.SelectItems, blank.SelectItems) ||
		whole.AfterComma != blank.AfterComma {
		t.Fatalf("AnalyzeQuery(%q) = %+v, AnalyzePartial with a blank %+v", query, whole, blank)
	}
}

// TestAnalyzeQuery checks the whole-query analysis on the corpus and that
// a table or log format typed last counts, unlike in AnalyzePartial,
// where it is the word under the cursor.
func TestAnalyzeQuery(t *testing.T) {
	for _, query := range append(slices.Clone(scanCorpus), validQueries...) {
		checkWholeQuery(t, query)
	}
	for _, query := range validQueries {
		q, err := NewQuery(query, logging.NopLogger{})
		if err != nil {
			t.Fatalf("NewQuery(%q): %v", query, err)
		}
		checkParsedAgreement(t, AnalyzeQuery(query), q)
	}
	p := AnalyzeQuery("select a from STATS")
	if p.Table != "STATS" || AnalyzePartial("select a from STATS").Table != "" {
		t.Fatalf("table typed last: %q", p.Table)
	}
	if p := AnalyzeQuery(`select a logformat "csv`); p.LogFormat != "csv" || p.InQuote {
		t.Fatalf("open quote at the end: logformat %q in quote %v", p.LogFormat, p.InQuote)
	}
}

// TestAnalyzePartialAfterComma checks that a comma before the word is
// reported, also with other blanks around it, and that commas inside a
// quote, before an earlier token or after the word's start do not count.
func TestAnalyzePartialAfterComma(t *testing.T) {
	for query, want := range map[string]bool{
		"":                                 false,
		",":                                true,
		"select a":                         false,
		"select a ":                        false,
		"select a,":                        true,
		"select a ,\t":                     true,
		"select a, co":                     true,
		"select a, count(":                 true,
		"select a,b c":                     false,
		`select a where b eq "x, y" `:      false,
		`select a where b eq "x, y",`:      true,
		`select a where b eq "x,`:          false,
		"select count(a), sum(b) where x ": false,
		"select a where b == 1 , and ":     false,
	} {
		p := AnalyzePartial(query)
		if p.AfterComma != want {
			t.Errorf("AnalyzePartial(%q).AfterComma = %v, want %v", query, p.AfterComma, want)
		}
	}
}

// checkScan compares scanQuery with tokenize on query and checks the offsets.
func checkScan(t *testing.T, query string) {
	t.Helper()
	scanned := scanQuery(query)
	var got []token
	for _, st := range scanned {
		got = append(got, st.token)
		var want string
		switch {
		case st.isBareword:
			want = st.str
		case st.open:
			want = `"` + st.str
		default:
			want = `"` + st.str + `"`
		}
		if st.start < 0 || st.end > len(query) || query[st.start:st.end] != want {
			t.Fatalf("scanQuery(%q): token %#v at [%d:%d], want source %q",
				query, st.token, st.start, st.end, want)
		}
	}
	if want := tokenize(query); !reflect.DeepEqual(got, want) {
		t.Fatalf("scanQuery(%q) = %#v, tokenize = %#v", query, got, want)
	}
}

// checkPartialInvariants checks what must hold for any input.
func checkPartialInvariants(t *testing.T, p Partial) {
	t.Helper()
	if p.WordStart < 0 || p.WordStart > len(p.Query) || p.Word != p.Query[p.WordStart:] {
		t.Fatalf("AnalyzePartial(%q): word %q at %d", p.Query, p.Word, p.WordStart)
	}
	end := 0
	for _, tok := range p.Tokens {
		if tok.Start < end || tok.End < tok.Start || tok.End > len(p.Query) ||
			p.Query[tok.Start:tok.End] != tok.Text {
			t.Fatalf("AnalyzePartial(%q): bad token %+v after offset %d", p.Query, tok, end)
		}
		end = tok.End
	}
}

// TestAnalyzePartialAgreesWithParser checks that AnalyzePartial reads a
// complete query as the parser does: same table, log format and select
// items, every piece known, and the same effective log format.
func TestAnalyzePartialAgreesWithParser(t *testing.T) {
	for _, query := range validQueries {
		q, err := NewQuery(query, logging.NopLogger{})
		if err != nil {
			t.Fatalf("NewQuery(%q): %v", query, err)
		}
		for _, suffix := range []string{"", " "} {
			p := AnalyzePartial(query + suffix)
			checkPartialInvariants(t, p)
			if suffix == " " {
				checkParsedAgreement(t, p, q)
			}
			for _, tok := range p.Tokens {
				if tok.Kind == TokenUnknown {
					t.Errorf("AnalyzePartial(%q): unknown token %q", p.Query, tok.Text)
				}
			}
		}
	}
}

func checkParsedAgreement(t *testing.T, p Partial, q *Query) {
	t.Helper()
	var items []string
	for _, sc := range q.Select {
		items = append(items, sc.FieldStorage)
	}
	if !slices.Equal(p.SelectItems, items) {
		t.Errorf("%q: select items %v, parser %v", p.Query, p.SelectItems, items)
	}
	wantTable := q.Table
	if q.LogFormat == "csv" && p.Table == "" {
		wantTable = "" // the parser's csv default ".", not typed
	}
	if p.Table != wantTable || p.LogFormat != q.LogFormat {
		t.Errorf("%q: table %q logformat %q, parser %q %q", p.Query, p.Table,
			p.LogFormat, q.Table, q.LogFormat)
	}
	for _, def := range []string{"", "default", "mimecast"} {
		if got, want := p.EffectiveLogFormat(def), q.EffectiveLogFormat(def); got != want {
			t.Errorf("%q: EffectiveLogFormat(%q) = %q, parser %q", p.Query, def, got, want)
		}
	}
	var keywords int
	for _, tok := range p.Tokens {
		if tok.Kind == TokenKeyword && (token{str: tok.Text, isBareword: true}).isKeyword() {
			keywords++
		}
	}
	var want int
	for _, tok := range tokenize(p.Query) {
		if tok.isKeyword() {
			want++
		}
	}
	if keywords != want {
		t.Errorf("%q: %d clause keyword tokens, tokenize has %d", p.Query, keywords, want)
	}
}

func TestAnalyzePartialExpect(t *testing.T) {
	const (
		kw   = ExpectClauseKeyword
		fld  = ExpectField
		agg  = ExpectAggregation
		item = fld | agg
	)
	tests := []struct {
		query  string
		expect Expect
		word   string
		clause string
	}{
		{"", kw, "", ""},
		{"   ", kw, "", ""},
		{"sel", kw, "sel", ""},
		{"select", kw, "select", ""},
		{"select ", item, "", "select"},
		{"select fo", item, "fo", "select"},
		{"select foo ", item | kw, "", "select"},
		{"select foo,", item | kw, "", "select"},
		{"select foo fr", item | kw, "fr", "select"},
		{"select foo from", item | kw, "from", "select"},
		{"select foo from ", ExpectTable, "", "from"},
		{"select foo from st", ExpectTable, "st", "from"},
		{"select foo from stats ", kw, "", "from"},
		{"select foo where ", fld, "", "where"},
		{"select foo where a ", ExpectWhereOperator, "", "where"},
		{"select foo where a e", ExpectWhereOperator, "e", "where"},
		{"select foo where a eq ", ExpectValue | fld, "", "where"},
		{"select foo where a eq 1 ", fld | ExpectAnd | kw, "", "where"},
		{"select foo where a eq \"x\"", fld | ExpectAnd | kw, "", "where"},
		{"select foo where a eq 1 and ", fld, "", "where"},
		{"select foo where a eq 1 an", fld | ExpectAnd | kw, "an", "where"},
		{"select foo group ", ExpectBy | fld, "", "group"},
		{"select foo group b", ExpectBy | fld, "b", "group"},
		{"select foo group by ", fld, "", "group"},
		{"select foo group by a ", fld | kw, "", "group"},
		{"select foo order ", ExpectBy | ExpectSelectItem, "", "order"},
		{"select foo order by ", ExpectSelectItem, "", "order"},
		{"select foo rorder by foo ", kw, "", "rorder"},
		{"select foo outfile ", ExpectAppend | ExpectOutfilePath, "", "outfile"},
		{"select foo outfile append ", ExpectOutfilePath, "", "outfile"},
		{"select foo outfile \"x.csv\" ", kw, "", "outfile"},
		{"select foo interval ", ExpectNumber, "", "interval"},
		{"select foo limit 5 ", kw, "", "limit"},
		{"select foo logformat ", ExpectLogFormat, "", "logformat"},
		{"select foo logformat gen", ExpectLogFormat, "gen", "logformat"},
		{"select foo set ", ExpectSetVariable, "", "set"},
		{"select foo set $a ", ExpectAssign, "", "set"},
		{"select foo set $a = ", ExpectSetFunction | fld, "", "set"},
		{"select foo set $a = md5sum(", ExpectSetFunction | fld, "", "set"},
		{"select foo set $a = md5sum(mask", ExpectSetFunction | fld, "mask", "set"},
		{"select foo set $a = md5sum(maskdigits($b", ExpectSetFunction | fld, "$b", "set"},
		{"select foo set $a = 1 ", ExpectSetVariable | kw, "", "set"},
		{"select count(", ExpectAggregationField, "", "select"},
		{"select foo,count($l", ExpectAggregationField, "$l", "select"},
		{"select count(foo)", item, "count(foo)", "select"},
		// Inside quotes and backticks no clause keyword can follow; inside
		// a double quote only the kinds a quoted token can be.
		{"select foo where a eq \"x y", ExpectValue, "x y", "where"},
		{"select foo outfile \"res", ExpectOutfilePath, "res", "outfile"},
		{"select \"fo", fld, "fo", "select"},
		{"select foo \"", fld, "", "select"},
		{"select foo from \"ST", ExpectTable, "ST", "from"},
		{"select foo logformat \"", ExpectLogFormat, "", "logformat"},
		{"select foo group \"", fld, "", "group"},
		{"select foo order \"", ExpectSelectItem, "", "order"},
		{"select foo order by \"fo", ExpectSelectItem, "fo", "order"},
		{"select foo limit \"", ExpectNumber, "", "limit"},
		{"select foo outfile \"", ExpectOutfilePath, "", "outfile"},
		{"select foo where \"", 0, "", "where"},
		{"select foo where a \"", 0, "", "where"},
		{"select foo where a eq 1 \"", 0, "", "where"},
		{"select foo set \"", 0, "", "set"},
		{"select foo set $a \"", 0, "", "set"},
		{"select foo set $a = \"", 0, "", "set"},
		{"select foo set $a = 1 \"", 0, "", "set"},
		{"select foo from stats \"", 0, "", "from"},
		// Only set functions open a call (the parser rejects foo().
		{"select foo set $a = foo(", ExpectSetFunction | fld, "foo(", "set"},
		{"select foo set $a = md5sum(foo(", ExpectSetFunction | fld, "md5sum(foo(", "set"},
		{"select `fr", fld, "fr", "select"},
		{"select foo `fr", fld, "fr", "select"},
		{"select foo group by `b", fld, "b", "group"},
		{"select `count(`", item, "`count(`", "select"},
		// Words before the first clause keyword are unknown.
		{"foo ", kw, "", ""},
	}
	for _, tc := range tests {
		p := AnalyzePartial(tc.query)
		checkPartialInvariants(t, p)
		if p.Expect != tc.expect || p.Word != tc.word || p.Clause != tc.clause {
			t.Errorf("AnalyzePartial(%q): expect %v word %q clause %q, want %v %q %q",
				tc.query, p.Expect, p.Word, p.Clause, tc.expect, tc.word, tc.clause)
		}
	}
}

func TestAnalyzePartialWordContext(t *testing.T) {
	p := AnalyzePartial("select foo where $a contains \"x")
	if !p.InQuote || p.InBacktick || p.InAggregation || p.WordStart != len(p.Query)-1 {
		t.Errorf("open quote: %+v", p)
	}
	if p.ValueField != "$a" || p.ValueOperator != (WhereOperator{"contains", StringOperator}) {
		t.Errorf("value context: field %q operator %+v", p.ValueField, p.ValueOperator)
	}
	// The value field is the escaped name without its backticks.
	p = AnalyzePartial("select foo where `a` >= ")
	if p.ValueField != "a" || p.ValueOperator.Kind != NumericOperator {
		t.Errorf("value field %q operator %+v", p.ValueField, p.ValueOperator)
	}
	// Backticks do not join words: `x is the left operand, y` an unknown
	// operator and >= the value, so no value is expected any more.
	p = AnalyzePartial("select foo where `x y` >= ")
	if p.ValueField != "" || p.Expect.Has(ExpectValue) {
		t.Errorf("split escape: value field %q expect %v", p.ValueField, p.Expect)
	}
	// A quoted left operand is a literal, not a field.
	p = AnalyzePartial("select foo where \"a\" eq ")
	if p.ValueField != "" || !p.Expect.Has(ExpectValue) {
		t.Errorf("quoted left operand: value field %q expect %v", p.ValueField, p.Expect)
	}
	// Only a valid aggregation name opens an aggregation.
	p = AnalyzePartial("select count)(fo")
	if p.InAggregation || p.Aggregation != "" || p.Word != "count)(fo" {
		t.Errorf("invalid aggregation: %+v", p)
	}
	p = AnalyzePartial("select foo where b =~ ")
	if p.ValueOperator.Name != "" || p.ValueField != "b" || !p.Expect.Has(ExpectValue) {
		t.Errorf("unknown operator: field %q operator %+v", p.ValueField, p.ValueOperator)
	}
	p = AnalyzePartial("select foo where b >= ")
	if p.ValueOperator != (WhereOperator{">=", NumericOperator}) {
		t.Errorf("numeric operator %+v", p.ValueOperator)
	}
	p = AnalyzePartial("select foo,sum(ba")
	if !p.InAggregation || p.Aggregation != "sum" || p.WordStart != len("select foo,sum(") {
		t.Errorf("aggregation: %+v", p)
	}
	p = AnalyzePartial("select `cou")
	if !p.InBacktick || p.InAggregation || p.WordStart != len("select `") {
		t.Errorf("backtick: %+v", p)
	}
	// A from clause in progress does not count yet; an earlier one does.
	p = AnalyzePartial("from a logformat generickv select foo from b")
	if p.Table != "A" || p.LogFormat != "generickv" || p.Word != "b" {
		t.Errorf("table %q logformat %q word %q", p.Table, p.LogFormat, p.Word)
	}
	if p.EffectiveLogFormat("x") != "generickv" {
		t.Errorf("effective log format %q", p.EffectiveLogFormat("x"))
	}
	p = AnalyzePartial("select a,count(b),`c` from x order by ")
	if want := []string{"a", "count(b)", "c"}; !slices.Equal(p.SelectItems, want) {
		t.Errorf("select items %v, want %v", p.SelectItems, want)
	}
	// A later select clause replaces the items, as in the parser.
	p = AnalyzePartial("select a select b order ")
	if !slices.Equal(p.SelectItems, []string{"b"}) {
		t.Errorf("select items %v, want [b]", p.SelectItems)
	}
	for _, query := range []string{"select foo", "select foo from", "select foo from "} {
		if got := AnalyzePartial(query).EffectiveLogFormat(""); got != "generic" {
			t.Errorf("%q: effective log format %q, want generic", query, got)
		}
	}
	if got := AnalyzePartial("select foo from x ").EffectiveLogFormat(""); got != "default" {
		t.Errorf("with from: effective log format %q, want default", got)
	}
}

// TestAnalyzePartialMarksRejectedTokens checks queries the parser rejects
// for one token: AnalyzePartial must mark a token of each as TokenUnknown.
func TestAnalyzePartialMarksRejectedTokens(t *testing.T) {
	for _, query := range []string{
		"select count(foo)bar",
		"select foo interval 99999999999999",
		"select foo interval -1",
		"select foo limit -1",
		"select foo limit x",
		"select foo set $a = md5sum(x))",
		"select foo set $a = sha1(x)",
		"select foo set a = 1",
		"select foo set $a == 1",
		"select median(x)",
		"select count)(fo",
		"select count(fo",
		`select ""`,
		"select foo from a b",
		"select foo order by foo gro",
		"select foo rorder foo gro",
		"select foo interval 5 gro",
		"select foo limit 5 5",
		"select foo logformat csv gro",
		`select foo outfile "a" gro`,
		"select foo gropu by foo",
		"select foo group foo by",
		`select foo from ""`,
		"select foo where a =~ b",
		"select foo where a == \"1\"",
		`select foo outfile ""`,
		`select foo outfile "a" "b"`,
		"select foo outfile append:x.csv",
		"select foo outfile append append:x.csv",
		`select foo outfile "append:x.csv"`,
		"foo select x",
		`"select" x`,
		`"SeleCt"0`,
		"`select` x",
	} {
		if _, err := NewQuery(query, logging.NopLogger{}); err == nil {
			t.Errorf("NewQuery(%q) succeeded, want an error", query)
			continue
		}
		p := AnalyzePartial(query + " ")
		if !slices.ContainsFunc(p.Tokens, func(tok PartialToken) bool {
			return tok.Kind == TokenUnknown
		}) {
			t.Errorf("AnalyzePartial(%q): no unknown token in %v", query, p.Tokens)
		}
	}
}

type kindedText struct {
	text string
	kind TokenKind
}

func TestAnalyzePartialTokenKinds(t *testing.T) {
	tests := []struct {
		query string
		want  []kindedText
	}{
		{"select count($l),foo from st", []kindedText{{"select", TokenKeyword},
			{"count", TokenAggregation}, {"$l", TokenVariable}, {"foo", TokenField},
			{"from", TokenKeyword}, {"st", TokenTable}}},
		{`where a EQ "x" and b >= 1.5 , 2 < c`, []kindedText{{"where", TokenKeyword},
			{"a", TokenField}, {"EQ", TokenOperator}, {`"x"`, TokenString},
			{"and", TokenKeyword}, {"b", TokenField}, {">=", TokenOperator},
			{"1.5", TokenNumber}, {"2", TokenNumber}, {"<", TokenOperator},
			{"c", TokenField}}},
		{"set $a = md5sum(maskdigits($b)), $c = \"s\" $d = `x` $e = 2", []kindedText{
			{"set", TokenKeyword}, {"$a", TokenVariable}, {"=", TokenOperator},
			{"md5sum", TokenFunction}, {"maskdigits", TokenFunction}, {"$b", TokenVariable},
			{"$c", TokenVariable}, {"=", TokenOperator}, {`"s"`, TokenString},
			{"$d", TokenVariable}, {"=", TokenOperator}, {"`x`", TokenField},
			{"$e", TokenVariable}, {"=", TokenOperator}, {"2", TokenNumber}}},
		{"group by a order by count(b) interval 5 limit 2 logformat csv outfile append \"o\"",
			[]kindedText{{"group", TokenKeyword}, {"by", TokenKeyword}, {"a", TokenField},
				{"order", TokenKeyword}, {"by", TokenKeyword}, {"count", TokenAggregation},
				{"b", TokenField}, {"interval", TokenKeyword}, {"5", TokenNumber},
				{"limit", TokenKeyword}, {"2", TokenNumber}, {"logformat", TokenKeyword},
				{"csv", TokenLogFormat}, {"outfile", TokenKeyword}, {"append", TokenKeyword},
				{`"o"`, TokenString}}},
		// Negative cases: what the parser rejects is unknown.
		{"foo select median(x),`y`,count(a)b", []kindedText{{"foo", TokenUnknown},
			{"select", TokenKeyword}, {"median(x)", TokenUnknown}, {"`y`", TokenField},
			{"count(a)b", TokenUnknown}}},
		// A set value is a function call only if it ends with ")".
		{"set $a = md5sum(x)b $b = md5sum(x $c = md5sum(x))", []kindedText{
			{"set", TokenKeyword}, {"$a", TokenVariable}, {"=", TokenOperator},
			{"md5sum(x)b", TokenField}, {"$b", TokenVariable}, {"=", TokenOperator},
			{"md5sum(x", TokenField}, {"$c", TokenVariable}, {"=", TokenOperator},
			{"md5sum(x))", TokenUnknown}}},
		{`select "" from a b where x =~ 1 set a = sha1(x) $b == 1`, []kindedText{
			{"select", TokenKeyword}, {`""`, TokenUnknown}, {"from", TokenKeyword},
			{"a", TokenTable}, {"b", TokenUnknown}, {"where", TokenKeyword},
			{"x", TokenField}, {"=~", TokenUnknown}, {"1", TokenNumber},
			{"set", TokenKeyword}, {"a", TokenUnknown}, {"=", TokenOperator},
			{"sha1(x)", TokenUnknown}, {"$b", TokenVariable},
			{"==", TokenUnknown}, {"1", TokenNumber}}},
		// A quoted or backticked keyword starts no clause.
		{`"select" foo "from" STATS`, []kindedText{{`"select"`, TokenUnknown},
			{"foo", TokenUnknown}, {`"from"`, TokenUnknown}, {"STATS", TokenUnknown}}},
		{"`select`0 select a", []kindedText{{"`select`0", TokenUnknown},
			{"select", TokenKeyword}, {"a", TokenField}}},
		{"interval 99999999999999 limit 99999999999999", []kindedText{
			{"interval", TokenKeyword}, {"99999999999999", TokenUnknown},
			{"limit", TokenKeyword}, {"99999999999999", TokenNumber}}},
		{"limit x interval -1 outfile \"\" logformat a b order by x y", []kindedText{
			{"limit", TokenKeyword}, {"x", TokenUnknown}, {"interval", TokenKeyword},
			{"-1", TokenUnknown}, {"outfile", TokenKeyword}, {`""`, TokenUnknown},
			{"logformat", TokenKeyword}, {"a", TokenLogFormat}, {"b", TokenUnknown},
			{"order", TokenKeyword}, {"by", TokenKeyword}, {"x", TokenField},
			{"y", TokenUnknown}}},
		// A string operator takes a backticked number as a field, a
		// numeric one as a number.
		{"where `1` eq `2` `3` == `4`", []kindedText{{"where", TokenKeyword},
			{"`1`", TokenField}, {"eq", TokenOperator}, {"`2`", TokenField},
			{"`3`", TokenNumber}, {"==", TokenOperator}, {"`4`", TokenNumber}}},
		{"where a eq \"open", []kindedText{{"where", TokenKeyword}, {"a", TokenField},
			{"eq", TokenOperator}, {"\"open", TokenString}}},
	}
	for _, tc := range tests {
		p := AnalyzePartial(tc.query)
		checkPartialInvariants(t, p)
		var got []kindedText
		for _, tok := range p.Tokens {
			got = append(got, kindedText{tok.Text, tok.Kind})
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("AnalyzePartial(%q) tokens\n got %v\nwant %v", tc.query, got, tc.want)
		}
	}
}

// TestAnalyzePartialKnowsEveryClause checks that the partial walker has a
// case for every clause keyword: a keyword added to the parser's table
// without one expects nothing after it and fails here.
func TestAnalyzePartialKnowsEveryClause(t *testing.T) {
	for _, keyword := range Keywords() {
		p := AnalyzePartial("select a " + keyword + " ")
		if p.Clause != keyword || p.Expect == 0 {
			t.Errorf("%s: clause %q expect %v, want a case in partialWalker", keyword, p.Clause, p.Expect)
		}
	}
	var w partialWalker
	w.startClause("nosuchclause")
	w.step(scannedToken{token: token{str: "x", isBareword: true}, end: 1})
	if w.expect() != 0 || w.spans[0].Kind != TokenUnknown {
		t.Errorf("unknown clause: expect %v, span %+v", w.expect(), w.spans[0])
	}
}

func TestKindNames(t *testing.T) {
	if TokenLogFormat.String() != "logformat" || TokenKind(-1).String() != "invalid" ||
		TokenKind(99).String() != "invalid" {
		t.Error("TokenKind.String")
	}
	if got := (ExpectClauseKeyword | ExpectOutfilePath).String(); got != "clause-keyword|outfile-path" {
		t.Errorf("Expect.String = %q", got)
	}
	if Expect(0).String() != "" || Expect(0).Has(ExpectField) {
		t.Error("empty Expect")
	}
}

// TestAnalyzePartialCandidateWalks types queries word by word, each word a
// candidate of a kind AnalyzePartial expects at that point, and checks that
// every prefix is read without unknown tokens, that a bareword candidate
// typed so far is the Word with its kind still expected, and that every
// query that ends where a clause keyword may follow parses.
func TestAnalyzePartialCandidateWalks(t *testing.T) {
	rng := rand.New(rand.NewSource(94))
	for walk := 0; walk < 300; walk++ {
		query := candidateWalk(t, rng)
		if _, err := NewQuery(query, logging.NopLogger{}); err != nil {
			t.Fatalf("walk %d: NewQuery(%q): %v", walk, query, err)
		}
	}
}

// candidateWalk builds one query from candidates. Clause keywords are used
// once each, select first, so that order by picks a select item that stays.
func candidateWalk(t *testing.T, rng *rand.Rand) string {
	t.Helper()
	g := candidateGen{used: map[string]bool{}}
	query := ""
	for steps := 0; ; steps++ {
		p := AnalyzePartial(query)
		if p.Expect.Has(ExpectClauseKeyword) && query != "" && (steps > 25 || rng.Intn(6) == 0) {
			return query
		}
		kind := g.pickKind(p, rng)
		if kind == 0 {
			return query
		}
		word := g.candidate(kind, p, rng)
		checkTypedCandidate(t, query, word, kind)
		query += word + " "
	}
}

// checkTypedCandidate checks the prefix query+word, with the word being
// typed and after it.
func checkTypedCandidate(t *testing.T, query, word string, kind Expect) {
	t.Helper()
	for _, typed := range []string{query + word, query + word + " "} {
		p := AnalyzePartial(typed)
		for _, tok := range p.Tokens {
			if tok.Kind == TokenUnknown {
				t.Fatalf("AnalyzePartial(%q): unknown token %q (%v candidate)", typed, tok.Text, kind)
			}
		}
	}
	if strings.ContainsAny(word, "\"`(") {
		return
	}
	for n := 1; n <= len(word); n++ {
		p := AnalyzePartial(query + word[:n])
		if p.Word != word[:n] || !p.Expect.Has(kind) {
			t.Fatalf("AnalyzePartial(%q): word %q expect %v, want %q with %v",
				query+word[:n], p.Word, p.Expect, word[:n], kind)
		}
	}
}

// TestAnalyzePartialMultiByte checks that offsets are byte offsets.
func TestAnalyzePartialMultiByte(t *testing.T) {
	p := AnalyzePartial("select ü,count(äb")
	if p.WordStart != 16 || p.Word != "äb" || !p.InAggregation || p.Aggregation != "count" {
		t.Errorf("word %q at %d, aggregation %v %q", p.Word, p.WordStart, p.InAggregation,
			p.Aggregation)
	}
	want := []PartialToken{{0, 6, "select", TokenKeyword}, {7, 9, "ü", TokenField},
		{10, 19, "count(äb", TokenUnknown}}
	if !reflect.DeepEqual(p.Tokens, want) {
		t.Errorf("tokens %v, want %v", p.Tokens, want)
	}
	p = AnalyzePartial("where ä eq \"ö ü")
	if !p.InQuote || p.WordStart != 13 || p.Word != "ö ü" || p.ValueField != "ä" {
		t.Errorf("quoted multi-byte word %q at %d, field %q", p.Word, p.WordStart, p.ValueField)
	}
}

type candidateGen struct {
	used map[string]bool
	n    int
}

// pickKind picks an expected kind; a clause keyword only while unused ones
// are left. It returns 0 if nothing can follow.
func (g *candidateGen) pickKind(p Partial, rng *rand.Rand) Expect {
	var kinds []Expect
	for i := range expectNames {
		kind := Expect(1) << i
		if !p.Expect.Has(kind) || kind == ExpectClauseKeyword && len(g.unusedKeywords()) == 0 {
			continue
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		return 0
	}
	return kinds[rng.Intn(len(kinds))]
}

func (g *candidateGen) unusedKeywords() []string {
	if !g.used["select"] {
		return []string{"select"}
	}
	var unused []string
	for _, keyword := range Keywords() {
		if !g.used[keyword] {
			unused = append(unused, keyword)
		}
	}
	return unused
}

// candidate returns a word of kind, unique where the parser needs it.
func (g *candidateGen) candidate(kind Expect, p Partial, rng *rand.Rand) string {
	g.n++
	field := [...]string{"f%d", "$v%d", "`f%d`"}[g.n%3]
	switch kind {
	case ExpectClauseKeyword:
		unused := g.unusedKeywords()
		keyword := unused[rng.Intn(len(unused))]
		g.used[keyword] = true
		if rng.Intn(2) == 0 {
			return strings.ToUpper(keyword)
		}
		return keyword
	case ExpectField:
		return g.field(p)
	case ExpectAggregation:
		names := AggregationNames()
		return fmt.Sprintf("%s("+field+")", names[rng.Intn(len(names))], g.n)
	case ExpectWhereOperator:
		ops := WhereOperators()
		return ops[rng.Intn(len(ops))].Name
	case ExpectValue:
		if p.ValueOperator.Kind == NumericOperator {
			return "1.5"
		}
		return `"x, y"`
	case ExpectSetFunction:
		names := funcs.Names()
		return fmt.Sprintf("%s(f%d)", names[rng.Intn(len(names))], g.n)
	case ExpectSelectItem:
		return orderItemWord(p.SelectItems[rng.Intn(len(p.SelectItems))])
	case ExpectSetVariable:
		return fmt.Sprintf("$s%d", g.n)
	}
	return fixedCandidates[kind]
}

// field returns a field name in one of the forms the parser takes as a
// field at p: plain, a $variable, in backticks, a clause keyword in
// backticks (each keyword once, so select items stay unique), and in a
// select or group clause a double-quoted name with a blank.
func (g *candidateGen) field(p Partial) string {
	forms := []string{"f%d", "$v%d", "`f%d`", "keyword"}
	if p.Clause == "select" || p.Clause == "group" {
		forms = append(forms, `"f %d"`)
	}
	form := forms[g.n%len(forms)]
	if form == "keyword" {
		keyword := Keywords()[g.n%len(keywords)]
		if g.used["field "+keyword] {
			return fmt.Sprintf("f%d", g.n)
		}
		g.used["field "+keyword] = true
		return "`" + keyword + "`"
	}
	return fmt.Sprintf(form, g.n)
}

// orderItemWord writes a select item (Partial.SelectItems) as order by
// needs it: double-quoted with a blank in it, a clause keyword or by in
// backticks, as the interactive completion writes it.
func orderItemWord(item string) string {
	lower := strings.ToLower(item)
	switch {
	case strings.ContainsAny(item, " ,"):
		return `"` + item + `"`
	case lower == "by" || slices.Contains(Keywords(), lower):
		return "`" + item + "`"
	}
	return item
}

var fixedCandidates = map[Expect]string{
	ExpectAnd: "and", ExpectBy: "by", ExpectAppend: "append", ExpectAssign: "=",
	ExpectTable: "stats", ExpectLogFormat: "generickv", ExpectNumber: "7",
	ExpectOutfilePath: `"out.csv"`,
}

// TestAnalyzePartialOutfileAppendColon: an outfile path starting with
// "append:" is unknown, as parseOutfileClause rejects it, with and without
// the append word and in quotes; the documented "./append:" spelling of a
// file really named so, and a path merely holding "append:", are strings.
func TestAnalyzePartialOutfileAppendColon(t *testing.T) {
	tests := []struct {
		query string
		want  TokenKind
	}{
		{"select foo outfile append:x.csv", TokenUnknown},
		{"select foo outfile append append:x.csv", TokenUnknown},
		{`select foo outfile "append:x.csv"`, TokenUnknown},
		{"select foo outfile append:", TokenUnknown},
		{"select foo outfile ./append:x.csv", TokenString},
		{"select foo outfile append ./append:x.csv", TokenString},
		{"select foo outfile xappend:x.csv", TokenString},
	}
	for _, tt := range tests {
		_, err := NewQuery(tt.query, logging.NopLogger{})
		if rejected := tt.want == TokenUnknown; (err != nil) != rejected {
			t.Errorf("NewQuery(%q) error = %v, want rejected %v", tt.query, err, rejected)
		}
		tokens := AnalyzePartial(tt.query + " ").Tokens
		if got := tokens[len(tokens)-1].Kind; got != tt.want {
			t.Errorf("AnalyzePartial(%q): path kind = %v, want %v", tt.query, got, tt.want)
		}
	}
}
