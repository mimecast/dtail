package clients

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// terminalText returns s as it may be written to the control TTY inside a
// message of dtail's own: every control and format character (hiddenRune:
// ESC, CR, tab, DEL, C1 controls, U+202E, U+2028, ...) becomes its Go
// escape (\x1b, \r, \t, \u202e) and every byte that is no valid UTF-8
// \xNN, so that such text can not drive the terminal, return the cursor
// over the message or reorder it. The line feed stays, for a message of
// dtail's own that has several lines; text that may come from a server
// goes through terminalLine first (errorLines), which leaves it none.
// Everything else (quotes, backslashes, non-ASCII text) is kept byte for
// byte, where displayText, which shows single names, would Go-quote the
// whole text. Escaping is idempotent.
func terminalText(s string) string {
	return escapeForTerminal(s, true)
}

// terminalLine is terminalText for text that must stay one line: the line
// feed becomes \n as well.
func terminalLine(s string) string {
	return escapeForTerminal(s, false)
}

// errorLines returns the message of err for the control TTY, one line per
// error: the errors of an errors.Join (the failures of several servers and
// of their rollbacks, also nested) each on a line of their own, and every
// one of them escaped as one line (terminalLine). So the reason a server
// gave for a rejected SESSION UPDATE, which is part of such an error, can
// not start a line: with a line feed it could otherwise forge control
// output ("x\nreload applied successfully"), which nothing tells apart
// from dtail's own without colors.
func errorLines(err error) string {
	parts := joinedErrors(err)
	lines := make([]string, len(parts))
	for i, part := range parts {
		lines[i] = terminalLine(part.Error())
	}
	return strings.Join(lines, "\n")
}

// joinedErrors returns the errors err joins, those of nested joins
// included, or err itself when it is no errors.Join: an error that wraps
// several (fmt.Errorf with two %w) has a message of its own and stays one.
func joinedErrors(err error) []error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	parts := joined.Unwrap()
	messages := make([]string, len(parts))
	for i, part := range parts {
		messages[i] = part.Error()
	}
	if len(parts) == 0 || strings.Join(messages, "\n") != err.Error() {
		return []error{err}
	}
	var flat []error
	for _, part := range parts {
		flat = append(flat, joinedErrors(part)...)
	}
	return flat
}

// escapeForTerminal escapes the hidden runes and invalid bytes of s, the
// line feed only unless keepLineFeed.
func escapeForTerminal(s string, keepLineFeed bool) string {
	escaped := func(r rune) bool { return hiddenRune(r) && (r != '\n' || !keepLineFeed) }
	if utf8.ValidString(s) && strings.IndexFunc(s, escaped) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case escaped(r):
			quoted := strconv.QuoteRune(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
