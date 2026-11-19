package mapr

import (
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

func FuzzTokenize(f *testing.F) {
	for _, seed := range []string{
		"select foo from bar",
		`select "free beer" from stats`,
		"select `from`,count(*) from stats",
		"\x00,\xff\"unterminated",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		first := tokenize(input)
		second := tokenize(input)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("tokenize(%q) is not deterministic: %#v != %#v", input, first, second)
		}
		for _, token := range first {
			if token.isBareword && token.str == "" {
				t.Fatalf("tokenize(%q) emitted an empty bareword", input)
			}
			if !token.isBareword && token.isKeyword() {
				t.Fatalf("quoted token %q was classified as a keyword", token.str)
			}
			if token.String() != token.str {
				t.Fatalf("token.String() = %q, want %q", token.String(), token.str)
			}
		}

		remaining, consumed := tokensConsume(first)
		if len(remaining)+len(consumed) > len(first) {
			t.Fatalf("tokensConsume expanded token stream: input=%d remaining=%d consumed=%d",
				len(first), len(remaining), len(consumed))
		}
		if len(remaining) > 0 && !remaining[0].isKeyword() {
			t.Fatalf("tokensConsume stopped at non-keyword %q", remaining[0].str)
		}
	})
}

func FuzzNewQuery(f *testing.F) {
	for _, seed := range []string{
		"select foo from bar",
		"from STATS select count(*) group by $hostname order by count(*) interval 5 limit 10",
		`select message from logs where level eq "error" outfile append "errors.csv"`,
		"select foo logformat csv",
		"select from",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		query, err := NewQuery(input, logging.NopLogger{})
		if err != nil {
			if query != nil {
				t.Fatalf("NewQuery(%q) returned query %#v with error %v", input, query, err)
			}
			return
		}
		if query == nil {
			t.Fatalf("NewQuery(%q) returned nil without an error", input)
		}
		if query.RawQuery != input {
			t.Fatalf("RawQuery = %q, want %q", query.RawQuery, input)
		}
		if len(query.Select) == 0 || len(query.GroupBy) == 0 {
			t.Fatalf("successful query has incomplete plan: %#v", query)
		}
		_ = query.String()
		_ = query.HasOutfile()
		_ = query.ParserFieldPlan()
		_ = query.ReferencedVariables()
	})
}
