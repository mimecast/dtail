package clients

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/omode"
)

var (
	queryArgs = config.Args{Mode: omode.MapClient, What: "/var/log/app.log",
		QueryStr: "from STATS select count($line) group by $hostname"}
	queryCatalog = catalogSnapshot{
		DefaultLogFormat: "default",
		Servers:          []string{"srv1"},
		Formats: map[string]catalogFormat{
			"default": {
				Tables: map[string]map[string][]string{
					"STATS": {"goroutines": {"12", "15"}, "lifetimeConnections": {"3"}},
					"OTHER": {"bytes": {"100"}},
				},
				Variables: map[string][]string{"$hostname": {"web1", "it's"}},
			},
		},
	}
)

func emptyCatalog() catalogSnapshot { return catalogSnapshot{} }

func catalogOf(c catalogSnapshot) func() catalogSnapshot { return func() catalogSnapshot { return c } }

// TestSplitInteractiveArgsPartial checks the partial tokenizer: the open
// quote of the current token and the raw offset of every decoded byte, also
// across escapes.
func TestSplitInteractiveArgsPartial(t *testing.T) {
	tests := []struct {
		raw     string
		tokens  []string
		current string
		quote   byte
		starts  []int
	}{
		{raw: ` --query 'sel`, tokens: []string{"--query"}, current: "sel", quote: '\'', starts: []int{10, 11, 12}},
		{raw: ` --query "a\"b`, tokens: []string{"--query"}, current: `a"b`, quote: '"', starts: []int{10, 11, 13}},
		{raw: ` --query=x\ y`, tokens: nil, current: "--query=x y", quote: 0},
		{raw: ` --query ''`, tokens: []string{"--query"}, current: "", quote: 0},
	}
	for _, tt := range tests {
		got := splitInteractiveArgsPartial(tt.raw)
		if !slices.Equal(got.tokens, tt.tokens) || got.current != tt.current || got.quote != tt.quote {
			t.Fatalf("%q: tokens %q current %q quote %q", tt.raw, got.tokens, got.current, got.quote)
		}
		if tt.starts != nil && !slices.Equal(got.rawStarts, tt.starts) {
			t.Fatalf("%q: raw starts %v, want %v", tt.raw, got.rawStarts, tt.starts)
		}
		if len(got.rawStarts) != len(got.current) {
			t.Fatalf("%q: %d raw starts for %d bytes", tt.raw, len(got.rawStarts), len(got.current))
		}
	}
	if got := splitInteractiveArgsPartial(` --query a\`); !got.escaped {
		t.Fatal("trailing backslash not reported as escaped")
	}
}

// TestEscapeForQuoteRoundTrip checks that every escaped text splits back to
// itself in its quote context.
func TestEscapeForQuoteRoundTrip(t *testing.T) {
	texts := []string{`select "web1" `, `it's`, `a\b`, `$line`, "x`y", `"q"`, "tab\there", "ünï"}
	for _, quote := range []byte{0, '\'', '"'} {
		for _, text := range texts {
			raw := escapeForQuote(text, quote)
			if quote != 0 {
				raw = string(quote) + raw + string(quote)
			}
			tokens, err := splitInteractiveArgs(raw)
			if err != nil || len(tokens) != 1 || tokens[0] != text {
				t.Fatalf("quote %q: %q encoded %q split to %q (%v)", quote, text, raw, tokens, err)
			}
		}
	}
}

// TestCompleteQueryValue covers Tab inside a --query value: every clause,
// both quote kinds, --query=..., values after string and numeric
// operators, and the escaping of what is inserted.
func TestCompleteQueryValue(t *testing.T) {
	tests := []struct {
		name, line, want string
		wantCandidates   []string // subset that must be listed
	}{
		{name: "keyword single quote", line: `:reload --query 'sel`, want: `:reload --query 'select `},
		{name: "keyword double quote", line: `:reload --query "sel`, want: `:reload --query "select `},
		{name: "keyword ignores case", line: `:reload --query 'SEL`, want: `:reload --query 'select `},
		{name: "unquoted escapes the blank", line: `:reload --query=sel`, want: `:reload --query=select\ `},
		{name: "aggregation", line: `:reload --query 'select cou`, want: `:reload --query 'select count(`},
		{name: "aggregation field from catalog", line: `:reload --query 'select count(gor`,
			want: `:reload --query 'select count(goroutines)`},
		{name: "field of the typed table", line: `:reload --query 'from STATS select li`,
			want: `:reload --query 'from STATS select lifetimeConnections `},
		{name: "table", line: `:reload --query 'select count($line) from ST`,
			want: `:reload --query 'select count($line) from STATS `},
		{name: "variable", line: `:reload --query 'select $host`, want: `:reload --query 'select $hostname `},
		{name: "operator list", line: `:reload --query 'select a from STATS where goroutines `,
			want: `:reload --query 'select a from STATS where goroutines `, wantCandidates: []string{"== ", "eq "}},
		// After an operator only values are offered (no fields), as bare
		// numbers after a numeric operator: here their common prefix.
		{name: "numeric values", line: `:reload --query 'select a from STATS where goroutines == `,
			want: `:reload --query 'select a from STATS where goroutines == 1`},
		{name: "numeric value prefix", line: `:reload --query 'select a from STATS where goroutines == 1`,
			want:           `:reload --query 'select a from STATS where goroutines == 1`,
			wantCandidates: []string{"12 ", "15 "}},
		{name: "string value single quote", line: `:reload --query 'select a where $hostname eq w`,
			want: `:reload --query 'select a where $hostname eq "web1" `},
		{name: "string value double quote", line: `:reload --query "select a where $hostname eq w`,
			want: `:reload --query "select a where $hostname eq \"web1\" `},
		{name: "value with apostrophe", line: `:reload --query 'select a where $hostname eq i`,
			want: `:reload --query 'select a where $hostname eq "it'\''s" `},
		{name: "by", line: `:reload --query 'from STATS select count($line) group b`,
			want: `:reload --query 'from STATS select count($line) group by `},
		{name: "logformat", line: `:reload --query 'select a logformat cs`, want: `:reload --query 'select a logformat csv `},
		{name: "set function", line: `:reload --query 'select a set $b = md5`, want: `:reload --query 'select a set $b = md5sum(`},
		// Negative cases.
		{name: "grep value is no query", line: `:reload --grep 'sel`, want: `:reload --grep 'sel`},
		{name: "unknown aggregation offers nothing", line: `:reload --query 'select median(`,
			want: `:reload --query 'select median(`},
		{name: "number expected", line: `:reload --query 'select a limit `, want: `:reload --query 'select a limit `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := completeInteractive(tt.line, len(tt.line), queryArgs, catalogOf(queryCatalog))
			if got.line != tt.want {
				t.Fatalf("line = %q, want %q (candidates %q)", got.line, tt.want, got.candidates)
			}
			for _, c := range tt.wantCandidates {
				if !slices.Contains(got.candidates, c) {
					t.Fatalf("candidates %q lack %q", got.candidates, c)
				}
			}
			// Without a from table the guide hints at choosing one.
			if got.hint != "" && (got.hint != tableFirstHint || strings.Contains(tt.line, "from ")) {
				t.Fatalf("hint %q with a filled catalog", got.hint)
			}
		})
	}
}

// TestCompleteQueryValueParses closes the quote of every completed query
// that is complete and checks that :reload tokenizes it into the intended
// query, which the parser accepts.
func TestCompleteQueryValueParses(t *testing.T) {
	for _, line := range []string{
		`:reload --query 'select a where $hostname eq i`,
		`:reload --query "select a where $hostname eq w`,
		`:reload --query 'select count(gor`,
	} {
		got := completeInteractive(line, len(line), queryArgs, catalogOf(queryCatalog))
		quote := line[len(":reload --query ")]
		args, err := splitInteractiveArgs(strings.TrimPrefix(got.line, ":reload") + string(quote))
		if err != nil || len(args) != 2 {
			t.Fatalf("%q: split %q, %v", got.line, args, err)
		}
		if _, err := mapr.NewQuery(args[1], nil); err != nil {
			t.Fatalf("%q: query %q does not parse: %v", got.line, args[1], err)
		}
	}
}

// TestCompleteQueryValueEmptyCatalog checks that the query language still
// completes without field data and that a hint says so where the catalog
// would have offered words, but not where it would not.
func TestCompleteQueryValueEmptyCatalog(t *testing.T) {
	got := completeInteractive(`:reload --query 'select `, len(`:reload --query 'select `), queryArgs, emptyCatalog)
	if got.hint != queryCatalogHint || !slices.Contains(got.candidates, "count(") {
		t.Fatalf("hint %q candidates %q", got.hint, got.candidates)
	}
	got = completeInteractive(`:reload --query 'sel`, len(`:reload --query 'sel`), queryArgs, emptyCatalog)
	if got.line != `:reload --query 'select ` || got.hint != "" {
		t.Fatalf("keyword without catalog: line %q hint %q", got.line, got.hint)
	}
}

// TestCompleteQueryValueNotStarted keeps the earlier behaviour: right after
// "--query " Tab inserts the current query.
func TestCompleteQueryValueNotStarted(t *testing.T) {
	got := completeInteractive(":reload --query ", len(":reload --query "), queryArgs, catalogOf(queryCatalog))
	if got.line != ":reload --query '"+queryArgs.QueryStr+"'" {
		t.Fatalf("line = %q", got.line)
	}
}

func catalogWith(tables map[string]map[string][]string) func() catalogSnapshot {
	return catalogOf(catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {Tables: tables}}})
}

// TestCompleteQueryValueCommonPrefixIsDecoded covers a common prefix taken
// over values that differ only after an escaped character: the prefix is
// taken on the decoded text, so the line never ends in a lone escaping
// backslash that would swallow the next flag.
func TestCompleteQueryValueCommonPrefixIsDecoded(t *testing.T) {
	catalog := catalogWith(map[string]map[string][]string{"T": {"s": {"a", "a b"}}})
	line := `:reload --query=from\ T\ select\ s\ where\ s\ eq\ \"a --files x`
	pos := strings.Index(line, " --files")
	got := completeInteractive(line, pos, queryArgs, catalog)
	tokens, err := splitInteractiveArgs(strings.TrimPrefix(got.line, ":reload"))
	if err != nil || len(tokens) != 3 || tokens[1] != "--files" || tokens[2] != "x" {
		t.Fatalf("line %q splits to %q (%v): --files swallowed", got.line, tokens, err)
	}
	// The value is inside the query's open string quote, so the decoded
	// candidates complete it with the closing quote.
	if !slices.Contains(got.candidates, `a b" `) {
		t.Fatalf("candidates %q, want the decoded values listed", got.candidates)
	}

	// Inside double quotes, with the cursor before the closing quote.
	catalog = catalogWith(map[string]map[string][]string{"T": {"s": {"x", "x$"}}})
	line = `:reload --query "from T select s where s eq x" --files f`
	pos = strings.Index(line, `" --files`)
	got = completeInteractive(line, pos, queryArgs, catalog)
	tokens, err = splitInteractiveArgs(strings.TrimPrefix(got.line, ":reload"))
	if err != nil || len(tokens) != 4 || tokens[2] != "--files" || !strings.HasSuffix(tokens[1], `eq "x`) {
		t.Fatalf("line %q splits to %q (%v)", got.line, tokens, err)
	}
}

// TestCompleteQueryValueKeepsUTF8 checks that a common prefix of
// multi-byte names is cut back to a whole character.
func TestCompleteQueryValueKeepsUTF8(t *testing.T) {
	catalog := catalogWith(map[string]map[string][]string{"ÄA": {"x": nil}, "ÖB": {"y": nil}})
	line := `:reload --query 'select a from `
	got := completeInteractive(line, len(line), queryArgs, catalog)
	if !utf8.ValidString(got.line) || got.line != line {
		t.Fatalf("line %q (valid UTF-8 %v), want it unchanged with the tables listed", got.line, utf8.ValidString(got.line))
	}
	if !slices.Contains(got.candidates, "ÄA ") || !slices.Contains(got.candidates, "ÖB ") {
		t.Fatalf("candidates %q", got.candidates)
	}
}

// TestQueryFieldWords checks how catalog names are written as fields: a
// keyword in backticks (which the parser strips), names the tokenizer
// would split or misread left out.
func TestQueryFieldWords(t *testing.T) {
	got := queryFieldWords([]string{"from", "ORDER", "user name", "a,b", `q"x`, "f(x)", "t`x", "", "ok"}, false)
	want := []string{"`from`", "`ORDER`", "ok"}
	if !slices.Equal(got, want) {
		t.Fatalf("words = %q, want %q", got, want)
	}
	if got := queryFieldWords([]string{"from"}, true); !slices.Equal(got, []string{"from"}) {
		t.Fatalf("inside backticks: %q", got)
	}
	if _, err := mapr.NewQuery("from STATS select `from`,count(`order`) group by `from`", nil); err != nil {
		t.Fatalf("backticked keyword fields do not parse: %v", err)
	}
}

// TestCompleteQueryValueNoFieldsInStringQuote checks that inside a query
// string quote of a where condition no field is offered (it would become a
// string literal), only values.
func TestCompleteQueryValueNoFieldsInStringQuote(t *testing.T) {
	line := `:reload --query 'from STATS select a where $hostname eq "$hos`
	got := completeInteractive(line, len(line), queryArgs, catalogOf(queryCatalog))
	if got.line != line || len(got.candidates) != 0 {
		t.Fatalf("line %q candidates %q: a field was offered inside the string quote", got.line, got.candidates)
	}
	line = `:reload --query 'from STATS select a where $hostname eq "w`
	got = completeInteractive(line, len(line), queryArgs, catalogOf(queryCatalog))
	if got.line != `:reload --query 'from STATS select a where $hostname eq "web1" ` {
		t.Fatalf("value inside the quote: %q", got.line)
	}
}

// TestCompleteQueryValueDoubleQuotedVariables checks that $variables typed
// unescaped inside double quotes (the shell keeps $ literal there) complete.
func TestCompleteQueryValueDoubleQuotedVariables(t *testing.T) {
	for line, want := range map[string]string{
		`:reload --query "from STATS select count($li`:    `:reload --query "from STATS select count(\$line)`,
		`:reload --query "from STATS select a where $hos`: `:reload --query "from STATS select a where \$hostname `,
	} {
		got := completeInteractive(line, len(line), queryArgs, catalogOf(queryCatalog))
		if got.line != want {
			t.Fatalf("%q: line %q, want %q", line, got.line, want)
		}
		tokens, err := splitInteractiveArgs(strings.TrimPrefix(got.line, ":reload") + `"`)
		if err != nil || !strings.Contains(tokens[1], "$") || strings.Contains(tokens[1], `\$`) {
			t.Fatalf("%q decodes to %q (%v)", got.line, tokens, err)
		}
	}
}

// TestQueryValueCandidatesFilter checks the values offered after operators:
// only plain decimal numbers after a numeric one, no value with a line
// break, and a sample of the probe's full length (possibly cut) only for
// operators comparing a part.
func TestQueryValueCandidatesFilter(t *testing.T) {
	long := strings.Repeat("v", logformat.ProbeMaxValueBytes)
	catalog := catalogWith(map[string]map[string][]string{"T": {
		"n": {"12", "NaN", "inf", "0x1p3", "-1.5e3"},
		"s": {long, "a\nb", "ok"},
	}})
	for query, want := range map[string][]string{
		`from T select n where n > `:        {"12 ", "-1.5e3 "},
		`from T select s where s eq `:       {`"ok" `},
		`from T select s where s contains `: {`"` + long + `" `, `"ok" `},
	} {
		// A value position offers values only, no fields or $variables.
		got := completeQueryWord(query, "", catalog())
		if !slices.Equal(got.candidates, want) {
			t.Fatalf("%q: values %q, want %q", query, got.candidates, want)
		}
	}
}

// TestCompleteQueryKeywordFieldInCalls checks that a field named like a
// keyword is written plain inside AGG( and a function call (the parser keeps
// backticks there, so `from` would be another field) and in backticks
// elsewhere, also when typed without the backtick, and that each result
// parses to the field from.
func TestCompleteQueryKeywordFieldInCalls(t *testing.T) {
	catalog := catalogWith(map[string]map[string][]string{"STATS": {"from": {"1"}}})
	for line, want := range map[string]string{
		`:reload --query 'from STATS select count(f`:             `:reload --query 'from STATS select count(from)`,
		`:reload --query 'from STATS select a set $x = md5sum(f`: `:reload --query 'from STATS select a set $x = md5sum(from)`,
		`:reload --query 'from STATS select a group by fro`:      `:reload --query 'from STATS select a group by ` + "`from` ",
	} {
		got := completeInteractive(line, len(line), queryArgs, catalog)
		if got.line != want {
			t.Fatalf("%q: line %q, want %q (candidates %q)", line, got.line, want, got.candidates)
		}
	}
	for _, query := range []string{"from STATS select count(from)", "from STATS select a set $x = md5sum(from)",
		"from STATS select a group by `from`"} {
		q, err := mapr.NewQuery(query, nil)
		if err != nil {
			t.Fatalf("%q does not parse: %v", query, err)
		}
		if strings.Contains(fmt.Sprintf("%+v", q.Select)+fmt.Sprintf("%+v", q.GroupBy), "`") {
			t.Fatalf("%q kept a backtick in a field: select %+v group %+v", query, q.Select, q.GroupBy)
		}
	}
}

// TestCompleteQueryValueHidesControlText checks that catalog text (log
// content: tables, fields, $variables, sample values) with a control or
// format character is never offered, as a candidate is printed in the
// listing and inserted into the edit line, where an escape sequence would
// run on the terminal; printable non-ASCII names still complete.
func TestCompleteQueryValueHidesControlText(t *testing.T) {
	const esc = "\x1b[31mX\x1b[0m"
	catalog := catalogOf(catalogSnapshot{DefaultLogFormat: "default", Servers: []string{"srv1"},
		Formats: map[string]catalogFormat{"default": {
			Tables: map[string]map[string][]string{
				"T": {"größe": {"ok", "e" + esc, "csi\u009b2J", "bidi\u202ex", "tab\tx", "ls\u2028x"},
					"f" + esc: nil},
				"U" + esc:      {"x": nil},
				"V\u200bwidth": {"y": nil},
			},
			Variables: map[string][]string{"$v" + esc: {"1"}, "$vär": {"2"}},
		}}})
	for line, want := range map[string][]string{
		`:reload --query 'select a from `:                    {"T "},
		`:reload --query 'from T select `:                    {"größe ", "$vär "},
		`:reload --query 'from T select a where größe eq `:   {`"ok" `},
		`:reload --query 'from T select a where größe eq "e`: nil,
	} {
		got := completeInteractive(line, len(line), queryArgs, catalog)
		for _, c := range got.candidates {
			if strings.IndexFunc(c, hiddenRune) >= 0 {
				t.Errorf("%q: offered %q with a control or format character", line, c)
			}
		}
		for _, w := range want {
			// A sole candidate is inserted into the line, not listed.
			if !slices.Contains(got.candidates, w) && got.line != line+w {
				t.Errorf("%q: %q not offered (line %q, candidates %q)", line, w, got.line, got.candidates)
			}
		}
		if strings.IndexFunc(got.line, hiddenRune) >= 0 {
			t.Errorf("%q: line %q holds a control or format character", line, got.line)
		}
	}
}
