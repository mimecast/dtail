package mapr

import (
	"reflect"
	"testing"
)

func TestTokensConsumeEmptyLiterals(t *testing.T) {
	want := []token{{str: "foo", isBareword: true}, {str: "eq", isBareword: true}, {str: "", isBareword: false}}
	tokens := tokenize(`foo eq "" select bar`)
	if len(tokens) != 5 {
		t.Fatalf("tokenize returned %d tokens, want 5: %#v", len(tokens), tokens)
	}
	if !reflect.DeepEqual(tokens[:3], want) {
		t.Fatalf("tokenize lost literal metadata: got %#v, want %#v", tokens, want)
	}
	remaining, got := tokensConsume(tokens)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokensConsume = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(remaining, tokens[3:]) {
		t.Errorf("remaining tokens = %#v, want %#v", remaining, tokens[3:])
	}
}

// TestTokensConsumeBacktickStripping covers the backtick-stripping path in
// tokensConsume, including the regression for a lone backtick token which
// previously panicked with "slice bounds out of range [1:0]".
func TestTokensConsumeBacktickStripping(t *testing.T) {
	type expected struct {
		str            string
		quotesStripped bool
	}
	tests := []struct {
		name  string
		input string
		want  []expected
	}{
		{
			name:  "empty string",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only",
			input: " \t\n ",
			want:  nil,
		},
		{
			name:  "single backtick",
			input: "`",
			want:  []expected{{str: "`", quotesStripped: false}},
		},
		{
			name:  "double backtick",
			input: "``",
			want:  []expected{{str: "", quotesStripped: true}},
		},
		{
			name:  "backtick quoted identifier",
			input: "`x`",
			want:  []expected{{str: "x", quotesStripped: true}},
		},
		{
			name:  "unterminated backtick",
			input: "`foo",
			want:  []expected{{str: "`foo", quotesStripped: false}},
		},
		{
			name:  "regular identifier",
			input: "foo",
			want:  []expected{{str: "foo", quotesStripped: false}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("tokenize/tokensConsume panicked on %q: %v", tc.input, r)
				}
			}()

			tokens := tokenize(tc.input)
			_, got := tokensConsume(tokens)

			if len(got) != len(tc.want) {
				t.Fatalf("input %q: got %d tokens, want %d (tokens=%v)",
					tc.input, len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i].str != w.str {
					t.Errorf("input %q token %d: str=%q, want %q",
						tc.input, i, got[i].str, w.str)
				}
				if got[i].quotesStripped != w.quotesStripped {
					t.Errorf("input %q token %d: quotesStripped=%v, want %v",
						tc.input, i, got[i].quotesStripped, w.quotesStripped)
				}
			}
		})
	}
}
