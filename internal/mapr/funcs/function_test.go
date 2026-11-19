package funcs

import (
	"strings"
	"testing"
)

func TestFunctionStackValid(t *testing.T) {
	t.Parallel()

	type want struct {
		arg string
		// result of calling the returned function stack on the original input
		callResult string
	}

	cases := []struct {
		input string
		want  want
	}{
		{
			input: "md5sum($line)",
			want:  want{arg: "$line", callResult: "b38699013d79e50d9d122433753959c1"},
		},
		{
			input: "maskdigits(md5sum(maskdigits($line)))",
			want:  want{arg: "$line", callResult: ".fac.bbe..bb.........d...a.c..b."},
		},
		{
			// An argument containing nested parens that are balanced is valid.
			input: "md5sum($foo)",
			want:  want{arg: "$foo"},
		},
		{
			// Plain field with no function wrapper is a degenerate stack (empty).
			input: "$line",
			want:  want{arg: "$line"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			fs, arg, err := NewFunctionStack(tc.input)
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v (stack %v)", tc.input, err, fs)
			}
			if arg != tc.want.arg {
				t.Errorf("arg: got %q, want %q", arg, tc.want.arg)
			}
			if tc.want.callResult != "" {
				got := fs.Call(tc.input)
				if got != tc.want.callResult {
					t.Errorf("Call(%q) = %q, want %q", tc.input, got, tc.want.callResult)
				}
			}
		})
	}
}

// TestFunctionStackMalformed verifies that NewFunctionStack rejects expressions
// that are structurally invalid. Before the fix, several of these were silently
// accepted and produced wrong results.
func TestFunctionStackMalformed(t *testing.T) {
	t.Parallel()

	cases := []string{
		// Missing opening paren — no function call syntax at all.
		"md5sum$line)",
		// Known outer function but inner call is missing its closing paren.
		"md5sum(makedigits$line))",
		// Stray ')' inside the argument after stripping: "bar)baz" remains.
		// Before the fix this was silently accepted and produced wrong output.
		"md5sum(bar)baz)",
		// Input ends with '(' — no closing ')' so the loop never strips,
		// but the argument string itself contains an unclosed '('.
		// Before the fix this was accepted as a plain field literal.
		"foo(",
		// Empty outer call — the name portion is empty (index == 0) which
		// is caught by the existing index <= 0 guard.
		"()",
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			fs, _, err := NewFunctionStack(input)
			if err == nil {
				t.Errorf("expected error for malformed input %q but got none (stack %v)", input, fs)
			}
		})
	}
}

// TestNamesResolve checks that every name Names lists (the ones the
// interactive completer offers) is a function NewFunctionStack accepts, and
// that names are case sensitive and unknown names are rejected.
func TestNamesResolve(t *testing.T) {
	t.Parallel()

	// The result of each function for the input "a1b2", written down
	// independently of functionTable so that a name mapped to the wrong
	// callback fails.
	want := map[string]string{
		"md5sum":     "f2e49af795161e14acf9d9245473a368",
		"maskdigits": "a.b.",
	}
	names := Names()
	if len(names) != len(want) {
		t.Errorf("Names() = %v, want the %d functions of the expected results", names, len(want))
	}
	for _, name := range names {
		fs, arg, err := NewFunctionStack(name + "($line)")
		if err != nil {
			t.Errorf("NewFunctionStack(%s($line)): %v", name, err)
			continue
		}
		if len(fs) != 1 || fs[0].Name != name || arg != "$line" {
			t.Errorf("NewFunctionStack(%s($line)) = %v, %q", name, fs, arg)
		}
		if result, ok := want[name]; !ok || fs.Call("a1b2") != result {
			t.Errorf("%s(\"a1b2\") = %q, want %q (expected result known: %v)",
				name, fs.Call("a1b2"), result, ok)
		}
		if _, _, err := NewFunctionStack(strings.ToUpper(name) + "($line)"); err == nil {
			t.Errorf("upper case %s was accepted, want case sensitive names", name)
		}
	}
	if _, _, err := NewFunctionStack("sha1($line)"); err == nil {
		t.Error("unknown function sha1 was accepted")
	}
}
