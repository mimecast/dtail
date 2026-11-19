package regex

import (
	"bytes"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// differentialInputs are used wherever a literal match is compared against what
// the compiled regexp matches.
var differentialInputs = []string{
	"",
	"ERROR",
	"an ERROR happened",
	"no match here",
	"INFO|0626-140021|1|stats.go:56|1|1|1|0.01|1h0m0s|MAPREDUCE:STATS|hostname=host1",
	"INFO|0626-140021|1|stats.go:56|1|1|1|0.01|1h0m0s|MAPREDUCE:OTHER|hostname=host1",
	`a\|b`,
	"a|b",
	"192.168.1.1",
	"192x168x1x1",
	"tab\there",
	"newline\nhere",
	"unicode: ÄÖÜ é 日本語",
	"invalid utf8: \xff\xfe",
	"replacement: \uFFFD",
	"backslash: \\ and more",
	"{braces} (parens) [brackets] ^caret$ plus+ star* question?",
}

func TestLiteralPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pattern     string
		wantLiteral string
		wantOK      bool
	}{
		// Patterns which are their own literal.
		{name: "word", pattern: "ERROR", wantLiteral: "ERROR", wantOK: true},
		{name: "with space", pattern: "hello world", wantLiteral: "hello world", wantOK: true},
		{name: "digits", pattern: "test123", wantLiteral: "test123", wantOK: true},
		{name: "path", pattern: "path/to/file", wantLiteral: "path/to/file", wantOK: true},
		{name: "assignment", pattern: "key=value", wantLiteral: "key=value", wantOK: true},
		{name: "dash", pattern: "JSON-data", wantLiteral: "JSON-data", wantOK: true},
		{name: "underscore", pattern: "_underscore_", wantLiteral: "_underscore_", wantOK: true},
		{name: "at sign", pattern: "user@example", wantLiteral: "user@example", wantOK: true},
		{name: "non ascii", pattern: "täst日本", wantLiteral: "täst日本", wantOK: true},
		{name: "empty", pattern: "", wantLiteral: "", wantOK: true},

		// Escaped punctuation unescapes to a literal.
		{name: "mapreduce filter", pattern: `\|MAPREDUCE:STATS\|`, wantLiteral: "|MAPREDUCE:STATS|", wantOK: true},
		{name: "mapreduce filter any table", pattern: `\|MAPREDUCE:\|`, wantLiteral: "|MAPREDUCE:|", wantOK: true},
		{name: "escaped dot", pattern: `192\.168\.1\.1`, wantLiteral: "192.168.1.1", wantOK: true},
		{name: "escaped backslash", pattern: `C:\\Users`, wantLiteral: `C:\Users`, wantOK: true},
		{name: "escaped parens", pattern: `\(group\)`, wantLiteral: "(group)", wantOK: true},
		{name: "escaped brackets", pattern: `\[abc\]`, wantLiteral: "[abc]", wantOK: true},
		{name: "escaped braces", pattern: `\{3\}`, wantLiteral: "{3}", wantOK: true},
		{name: "escaped quantifiers", pattern: `a\+b\*c\?`, wantLiteral: "a+b*c?", wantOK: true},
		{name: "escaped anchors", pattern: `\^start\$`, wantLiteral: "^start$", wantOK: true},
		{name: "escaped dash", pattern: `a\-b`, wantLiteral: "a-b", wantOK: true},
		{name: "escaped slash", pattern: `a\/b`, wantLiteral: "a/b", wantOK: true},
		{name: "escaped underscore", pattern: `a\_b`, wantLiteral: "a_b", wantOK: true},
		{name: "escaped punctuation run", pattern: `\!\"\#\%\&\'\,\:\;\<\=\>\@\~` + "\\`", wantLiteral: "!\"#%&',:;<=>@~`", wantOK: true},

		// Unescaped metacharacters.
		{name: "any", pattern: ".*", wantOK: false},
		{name: "trailing any", pattern: "test.*", wantOK: false},
		{name: "unescaped dot", pattern: "test.log", wantOK: false},
		{name: "start anchor", pattern: "^start", wantOK: false},
		{name: "end anchor", pattern: "end$", wantOK: false},
		{name: "class", pattern: "[abc]", wantOK: false},
		{name: "plus", pattern: "a+b", wantOK: false},
		{name: "question", pattern: "a?b", wantOK: false},
		{name: "star", pattern: "a*b", wantOK: false},
		{name: "group", pattern: "(group)", wantOK: false},
		{name: "alternation", pattern: "a|b", wantOK: false},
		{name: "repetition", pattern: "test{3}", wantOK: false},
		{name: "flags", pattern: "(?i)error", wantOK: false},
		{name: "non capturing group", pattern: "(?:ERROR)", wantOK: false},
		{name: "mixed escaped and unescaped", pattern: `\|MAPREDUCE:.*\|`, wantOK: false},

		// Escapes with a meaning of their own.
		{name: "digit class", pattern: `\d`, wantOK: false},
		{name: "non digit class", pattern: `\D`, wantOK: false},
		{name: "word class", pattern: `\w+`, wantOK: false},
		{name: "space class", pattern: `\s`, wantOK: false},
		{name: "non space class", pattern: `\S`, wantOK: false},
		{name: "word boundary", pattern: `\bword\b`, wantOK: false},
		{name: "non word boundary", pattern: `\B`, wantOK: false},
		{name: "text start", pattern: `\Astart`, wantOK: false},
		{name: "text end", pattern: `end\z`, wantOK: false},
		{name: "quoted span", pattern: `\Qa.b\E`, wantOK: false},
		{name: "newline escape", pattern: `a\nb`, wantOK: false},
		{name: "tab escape", pattern: `a\tb`, wantOK: false},
		{name: "bell escape", pattern: `a\ab`, wantOK: false},
		{name: "form feed escape", pattern: `a\fb`, wantOK: false},
		{name: "carriage return escape", pattern: `a\rb`, wantOK: false},
		{name: "vertical tab escape", pattern: `a\vb`, wantOK: false},
		{name: "hex escape", pattern: `\x41`, wantOK: false},
		{name: "braced hex escape", pattern: `\x{263a}`, wantOK: false},
		{name: "octal escape", pattern: `\012`, wantOK: false},
		{name: "zero escape", pattern: `\0`, wantOK: false},
		{name: "backreference like escape", pattern: `\1`, wantOK: false},
		{name: "unicode class", pattern: `\p{L}`, wantOK: false},
		{name: "negated unicode class", pattern: `\P{L}`, wantOK: false},
		{name: "any byte", pattern: `\C`, wantOK: false},
		{name: "escaped space", pattern: `a\ b`, wantOK: false},
		{name: "escaped non ascii", pattern: `\é`, wantOK: false},
		{name: "trailing backslash", pattern: `abc\`, wantOK: false},
		{name: "lone backslash", pattern: `\`, wantOK: false},

		// Input the byte search and the regexp engine disagree about.
		{name: "invalid utf8", pattern: "abc\xff", wantOK: false},
		{name: "replacement rune", pattern: "abc\uFFFD", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotLiteral, gotOK := literalPattern(tt.pattern)
			if gotOK != tt.wantOK {
				t.Fatalf("literalPattern(%q) ok = %v, want %v", tt.pattern, gotOK, tt.wantOK)
			}
			if !gotOK {
				return
			}
			if gotLiteral != tt.wantLiteral {
				t.Fatalf("literalPattern(%q) literal = %q, want %q",
					tt.pattern, gotLiteral, tt.wantLiteral)
			}
			assertLiteralMatchesRegexp(t, tt.pattern, gotLiteral, differentialInputs)
		})
	}
}

// assertLiteralMatchesRegexp fails if searching for literal gives a different
// answer than the compiled pattern for any of the inputs.
func assertLiteralMatchesRegexp(t *testing.T, pattern, literal string, inputs []string) {
	t.Helper()

	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("pattern %q was accepted as the literal %q but does not compile: %v",
			pattern, literal, err)
	}
	for _, input := range inputs {
		if want, got := re.MatchString(input), strings.Contains(input, literal); got != want {
			t.Errorf("pattern %q, input %q: literal search = %v, regexp = %v",
				pattern, input, got, want)
		}
		if want, got := re.Match([]byte(input)), bytes.Contains([]byte(input), []byte(literal)); got != want {
			t.Errorf("pattern %q, input %q: literal byte search = %v, regexp = %v",
				pattern, input, got, want)
		}
	}
}

// TestLiteralEscapeAgreesWithRegexp pins the accepted escapes against regexp
// itself: every escape treated as a literal must compile and must match exactly
// what a search for the escaped character matches.
func TestLiteralEscapeAgreesWithRegexp(t *testing.T) {
	t.Parallel()

	inputs := []string{"", "abc", "A", "1", " ", "\t", "\n", "\x00", "é", "\uFFFD", "\xff"}

	for c := 0; c < 128; c++ {
		pattern := `\` + string(rune(c))
		literal, ok := literalPattern(pattern)
		if !ok {
			continue
		}
		if literal != string(rune(c)) {
			t.Errorf("escape %q unescaped to %q, want %q", pattern, literal, string(rune(c)))
			continue
		}
		caseInputs := append([]string{}, inputs...)
		caseInputs = append(caseInputs, literal, "x"+literal+"y", pattern)
		assertLiteralMatchesRegexp(t, pattern, literal, caseInputs)
	}

	// Alphanumeric escapes must never be taken as literals, whether they mean
	// something else today or are simply not valid.
	for _, c := range "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		if _, ok := literalPattern(`\` + string(c)); ok {
			t.Errorf("escape %q must not be treated as a literal", `\`+string(c))
		}
	}
}

// TestLiteralPatternDifferential compares the literal search with the compiled
// regexp for randomly assembled patterns and inputs.
func TestLiteralPatternDifferential(t *testing.T) {
	t.Parallel()

	fragments := []string{
		"a", "B", "7", "-", "_", ":", "|", ".", "*", "+", "?", "^", "$",
		"(", ")", "[", "]", "{", "}", `\|`, `\.`, `\*`, `\\`, `\-`, `\(`,
		`\d`, `\w`, `\s`, `\b`, `\Q`, `\E`, `\n`, `\x41`, `\p{L}`, `\1`,
		"MAPREDUCE", "STATS", " ", "é", "\uFFFD",
	}
	inputs := append([]string{}, differentialInputs...)
	inputs = append(inputs,
		"a|b.c*d", `a\b`, "aB7-_:|", "MAPREDUCE:STATS", "|MAPREDUCE:STATS|",
		"\\d", "x41", "A", "p{L}", "QE", "nnn")

	random := rand.New(rand.NewSource(20260916))
	accepted := 0

	for i := 0; i < 20000; i++ {
		var pattern strings.Builder
		for n := random.Intn(6); n >= 0; n-- {
			pattern.WriteString(fragments[random.Intn(len(fragments))])
		}
		literal, ok := literalPattern(pattern.String())
		if !ok {
			continue
		}
		accepted++
		assertLiteralMatchesRegexp(t, pattern.String(), literal, inputs)
	}

	if accepted == 0 {
		t.Fatal("no generated pattern was accepted as a literal, the test proves nothing")
	}
	t.Logf("compared %d generated literal patterns against regexp", accepted)
}

// FuzzLiteralPattern checks the same property for arbitrary patterns: whatever
// is accepted as a literal must compile and must match like the regexp does.
func FuzzLiteralPattern(f *testing.F) {
	seeds := []struct {
		pattern string
		input   string
	}{
		{`\|MAPREDUCE:STATS\|`, "INFO|MAPREDUCE:STATS|a=b"},
		{"ERROR", "an ERROR here"},
		{`192\.168\.1\.1`, "host 192.168.1.1 down"},
		{`\d+`, "42"},
		{`a\ b`, "a b"},
		{`C:\\Users`, `C:\Users\paul`},
		{"a|b", "b"},
		{"abc\xff", "abc\xff"},
	}
	for _, seed := range seeds {
		f.Add(seed.pattern, seed.input)
	}

	f.Fuzz(func(t *testing.T, pattern, input string) {
		// The reference side of this property (regexp.MatchString and
		// regexp.Match on a literal pattern) costs O(len(pattern) *
		// len(input)) in the worst case, so a large input which keeps
		// re-entering a partial match starves the fuzzer: a 64 KiB
		// pattern of repeated `\|` against 64 KiB of '|' spends about
		// 27.5 s matching on this machine and freezes the execution
		// counter for the rest of the run. The same pattern against
		// 64 KiB of 'a' costs about 12 ms per execution of this body,
		// and nearly all of that is the regexp.Compile below rather
		// than matching, so it is the pair which is expensive, not the
		// size alone. The bugs this target looks for (an escape
		// unescaped into the wrong bytes, a metacharacter slipping
		// through) all show up in short patterns, so bound both sides
		// and keep the execution rate high.
		if len(pattern) > 1024 || len(input) > 4096 {
			return
		}
		literal, ok := literalPattern(pattern)
		if !ok {
			return
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("pattern %q accepted as literal %q but does not compile: %v",
				pattern, literal, err)
		}
		if want, got := re.MatchString(input), strings.Contains(input, literal); got != want {
			t.Fatalf("pattern %q, input %q: literal search = %v, regexp = %v",
				pattern, input, got, want)
		}
		if want, got := re.Match([]byte(input)), bytes.Contains([]byte(input), []byte(literal)); got != want {
			t.Fatalf("pattern %q, input %q: literal byte search = %v, regexp = %v",
				pattern, input, got, want)
		}
	})
}

func TestLiteralMatching(t *testing.T) {
	tests := []struct {
		pattern string
		text    string
		match   bool
	}{
		{"ERROR", "This is an ERROR message", true},
		{"ERROR", "This is an error message", false}, // Case sensitive
		{"WARNING", "This is an ERROR message", false},
		{"test", "testing 123", true},
		{"test", "Test 123", false}, // Case sensitive
		{`\|MAPREDUCE:STATS\|`, "INFO|1|MAPREDUCE:STATS|a=b", true},
		{`\|MAPREDUCE:STATS\|`, "INFO|1|MAPREDUCE:OTHER|a=b", false},
		{`\|MAPREDUCE:STATS\|`, `INFO|1\|MAPREDUCE:STATS\|a=b`, false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			// Test with Default flag
			r, err := New(tt.pattern, Default)
			if err != nil {
				t.Fatalf("Failed to create regex: %v", err)
			}

			// Verify it's detected as literal
			if !r.isLiteral {
				t.Errorf("Pattern %q should be detected as literal", tt.pattern)
			}

			// Test string matching
			got := r.MatchString(tt.text)
			if got != tt.match {
				t.Errorf("MatchString(%q, %q) = %v, want %v", tt.pattern, tt.text, got, tt.match)
			}

			// Test byte matching
			gotBytes := r.Match([]byte(tt.text))
			if gotBytes != tt.match {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.text, gotBytes, tt.match)
			}
		})
	}

	// Test with Invert flag
	t.Run("InvertFlag", func(t *testing.T) {
		r, err := New(`\|MAPREDUCE:STATS\|`, Invert)
		if err != nil {
			t.Fatalf("Failed to create regex: %v", err)
		}

		if !r.isLiteral {
			t.Error("Pattern should be detected as literal")
		}

		// Should NOT match when pattern is present
		if r.MatchString("INFO|1|MAPREDUCE:STATS|a=b") {
			t.Error("Inverted match should return false when pattern is present")
		}

		// Should match when pattern is absent
		if !r.MatchString("This is a normal message") {
			t.Error("Inverted match should return true when pattern is absent")
		}
	})
}

func TestNonLiteralPatternsUseRegexp(t *testing.T) {
	t.Parallel()

	patterns := []string{`\d+`, "(?i)error", "ERROR|WARNING", "^INFO", `\bword\b`}

	for _, pattern := range patterns {
		r, err := New(pattern, Default)
		if err != nil {
			t.Fatalf("Failed to create regex %q: %v", pattern, err)
		}
		if r.IsLiteral() {
			t.Errorf("Pattern %q must not use literal matching", pattern)
		}
		for _, input := range differentialInputs {
			want := regexp.MustCompile(pattern).MatchString(input)
			if got := r.MatchString(input); got != want {
				t.Errorf("pattern %q, input %q: got %v, want %v", pattern, input, got, want)
			}
		}
	}
}

func TestSerializationWithLiteral(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pattern    string
		wantHint   bool
		wantLiter  bool
		matchTexts []string
	}{
		{
			name:       "plain literal keeps the wire hint",
			pattern:    "ERROR",
			wantHint:   true,
			wantLiter:  true,
			matchTexts: []string{"This is an ERROR message", "nothing here"},
		},
		{
			// Older peers trust the hint verbatim and would search for the
			// backslashes, so an unescaped literal must not carry it.
			name:       "escaped literal drops the wire hint",
			pattern:    `\|MAPREDUCE:STATS\|`,
			wantHint:   false,
			wantLiter:  true,
			matchTexts: []string{"INFO|1|MAPREDUCE:STATS|a=b", "INFO|1|MAPREDUCE:OTHER|a=b"},
		},
		{
			name:       "regexp pattern has no hint",
			pattern:    `\d+`,
			wantHint:   false,
			wantLiter:  false,
			matchTexts: []string{"count 42", "no digits"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := New(tt.pattern, Default)
			if err != nil {
				t.Fatalf("Failed to create regex: %v", err)
			}
			if r.IsLiteral() != tt.wantLiter {
				t.Fatalf("IsLiteral() = %v, want %v", r.IsLiteral(), tt.wantLiter)
			}

			serialized, err := r.Serialize()
			if err != nil {
				t.Fatalf("Failed to serialize: %v", err)
			}
			if gotHint := strings.Contains(serialized, "literal"); gotHint != tt.wantHint {
				t.Fatalf("serialized %q literal hint = %v, want %v",
					serialized, gotHint, tt.wantHint)
			}

			deserialized, err := Deserialize(serialized)
			if err != nil {
				t.Fatalf("Failed to deserialize: %v", err)
			}
			if deserialized.IsLiteral() != tt.wantLiter {
				t.Errorf("deserialized IsLiteral() = %v, want %v",
					deserialized.IsLiteral(), tt.wantLiter)
			}
			if deserialized.String() != r.String() {
				t.Errorf("deserialized regex = %s, want %s", deserialized.String(), r.String())
			}
			for _, text := range tt.matchTexts {
				if r.MatchString(text) != deserialized.MatchString(text) {
					t.Errorf("input %q: original and deserialized regex disagree", text)
				}
			}
		})
	}
}

// TestDeserializeIgnoresUntrustedLiteralHint makes sure a hint from a peer with
// a different notion of a literal cannot turn a pattern into the wrong search.
func TestDeserializeIgnoresUntrustedLiteralHint(t *testing.T) {
	t.Parallel()

	r, err := Deserialize(`regex:default,literal \d+`)
	if err != nil {
		t.Fatalf("Deserialize() error = %v", err)
	}
	if r.IsLiteral() {
		t.Fatal("a pattern which is not provably a literal must keep the compiled regexp")
	}
	if !r.MatchString("count 42") || r.MatchString("no digits here") {
		t.Fatal("deserialized regex does not match like its pattern")
	}
}

func ExampleRegex_IsLiteral() {
	r, err := New(`\|MAPREDUCE:STATS\|`, Default)
	if err != nil {
		panic(err)
	}
	fmt.Println(r.IsLiteral(), r.MatchString("INFO|1|MAPREDUCE:STATS|a=b"))
	// Output: true true
}
