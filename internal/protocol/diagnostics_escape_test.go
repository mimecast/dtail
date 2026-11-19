package protocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestDiagnosticDetailsEscapeControlCharacters checks that client-supplied
// text in a diagnostic detail (a requested path, a probe entry) can not
// forge log lines or terminal escapes: every control and format character is
// escaped, so a diagnostic stays one line.
func TestDiagnosticDetailsEscapeControlCharacters(t *testing.T) {
	tests := []struct {
		name   string
		detail any
		want   string
	}{
		{"newline", "/var/log/x\nERROR|0101-000000|forged", `/var/log/x\nERROR|0101-000000|forged`},
		{"carriage return", "a\rb", `a\rb`},
		{"tab", "a\tb", `a\tb`},
		{"escape", "a\x1b[31mred", `a\x1b[31mred`},
		{"nul and del", "a\x00b\x7f", `a\x00b\x7f`},
		{"c1 control", "a\u0085b\u009bc", `a\u0085b\u009bc`},
		{"bidi override", "evil\u202egol.txt", `evil\u202egol.txt`},
		{"line separator", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		// Lone bytes 0x80-0x9f are no valid UTF-8, but an 8-bit terminal
		// takes them for C1 controls (0x9b CSI, 0x85 NEL).
		{"lone c1 bytes", "a\x9b31mb\x85c", `a\x9b31mb\x85c`},
		{"truncated sequence ending in a c1 byte", "x\xe2\x82", "x\xe2" + `\x82`},
		{"error value", errors.New("open x\ny: denied"), `open x\ny: denied`},
		{"formatted value", []string{"a\nb"}, `[a\nb]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EncodeDiagnosticDetails([]any{test.detail})
			if got != test.want {
				t.Fatalf("encoded detail = %q, want %q", got, test.want)
			}
		})
	}
}

// TestDiagnosticDetailsKeepPrintableTextVerbatim checks that ordinary paths
// and messages, non-ASCII text and bytes that are no valid UTF-8 (a Latin-1
// file name) stay byte-identical, and that a backslash is not escaped, so
// the escaping never applies twice to an already escaped text.
func TestDiagnosticDetailsKeepPrintableTextVerbatim(t *testing.T) {
	for _, text := range []string{
		"",
		"/var/log/app/service.log",
		`C:\logs\a b.log`,
		"日本語/ログ €¬Ь 😀.log",
		"caf\xe9.log",
		"truncated \xe2",
		`already\nescaped`,
	} {
		got := EncodeDiagnosticDetails([]any{text})
		if got != text {
			t.Errorf("encoded detail = %q, want it verbatim", got)
		}
	}
}

// TestTimedDiagnosticWithNewlineIsOneLine is the log forging regression: a
// dserver diagnostic carrying a requested path with a newline is one line.
func TestTimedDiagnosticWithNewlineIsOneLine(t *testing.T) {
	path := "/tmp/x\nERROR|0101-000000|forged line"
	got := EncodeTimedDiagnostic(TimedDiagnostic{
		Level: "WARN", Timestamp: "0914-120000",
		Details: []any{"user", "Probe: invalid glob", path, fmt.Errorf("bad %q", "p")},
	})
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("diagnostic spans several lines: %q", got)
	}
}
