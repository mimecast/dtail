package clients

import (
	"errors"
	"strings"
)

// interactiveArgTokenizer splits the flags of an interactive `:reload` line
// into argument tokens with POSIX-shell-like quoting, so that a quoted or
// escaped value yields the same argument it would yield when typed on a
// shell command line (apart from expansion, see below):
//
//   - Outside quotes, a backslash makes the next character literal (`a\ b` is
//     one token "a b", `\d` is "d"), and spaces and tabs separate tokens.
//   - Single quotes keep everything up to the closing quote literally,
//     backslashes included (`'\d+'` is `\d+`). This is the quoting to use for
//     regular expressions.
//   - Double quotes keep backslashes literally, except that `\\`, `\"`, `\$`
//     and "\`" stand for a backslash, a double quote, a dollar sign and a
//     backtick (`"a\.b"` is `a\.b`, `"\$x"` is `$x`), as in a POSIX shell.
//     The shell's fifth case, a backslash-newline line continuation, cannot
//     occur because the control TTY delivers one line at a time.
//   - Quoted parts and unquoted parts join into one token when adjacent
//     (`--regex=foo' 'bar` is "--regex=foo bar"), and an empty quoted string
//     (a pair of single or of double quotes) is an empty argument, so that
//     `--regex ""` sets the regex to "". That is why the tokenizer tracks
//     whether a token was started separately from its length.
//
// There is no variable, command or glob expansion; `$` and `*` stay literal
// (where a shell would expand `$line` inside double quotes, it stays `$line`).
//
// The tokenizer walks the input byte by byte rather than rune by rune: every
// special character is ASCII and no byte of a multi-byte UTF-8 sequence is
// below 0x80, so non-ASCII text passes through unchanged. The tokenizer
// itself keeps invalid UTF-8 bytes as well, but parseInteractiveReload
// rejects such a line before tokenizing it, because the JSON-encoded
// SESSION UPDATE payload would replace them with U+FFFD.
type interactiveArgTokenizer struct {
	tokens  []string
	current strings.Builder
	// started is true once the current token began, even when it is still
	// empty because it consists of an empty quoted string only.
	started bool
	// quote is the quote byte of the quoted part being read, or 0 outside quotes.
	quote byte
	// escaped is true when the previous byte was an escaping backslash,
	// outside quotes or inside double quotes.
	escaped bool
}

// splitInteractiveArgs tokenizes the flags of an interactive `:reload` line,
// see interactiveArgTokenizer for the quoting rules. It fails on an
// unterminated quoted string or a trailing backslash.
func splitInteractiveArgs(raw string) ([]string, error) {
	var t interactiveArgTokenizer
	for i := 0; i < len(raw); i++ {
		t.consume(raw[i])
	}
	return t.finish()
}

func (t *interactiveArgTokenizer) consume(b byte) {
	switch {
	case t.escaped:
		t.consumeEscaped(b)
	case t.quote == '\'':
		t.consumeSingleQuoted(b)
	case t.quote == '"':
		t.consumeDoubleQuoted(b)
	default:
		t.consumeUnquoted(b)
	}
}

// consumeEscaped handles the byte after an escaping backslash. Outside quotes
// it is taken literally; for a multi-byte character only its first byte
// follows the backslash and the remaining bytes are plain bytes, so the whole
// character is kept either way.
func (t *interactiveArgTokenizer) consumeEscaped(b byte) {
	t.escaped = false
	// Inside double quotes a backslash escapes only the characters that are
	// special there; before any other character both are kept, as in a POSIX
	// shell.
	if t.quote == '"' && !isDoubleQuoteEscapable(b) {
		t.current.WriteByte('\\')
	}
	t.current.WriteByte(b)
}

// isDoubleQuoteEscapable reports whether a backslash inside double quotes
// escapes b: a POSIX shell removes the backslash before `\`, `"`, `$` and "`"
// there (and before a newline, which a single control line cannot contain).
func isDoubleQuoteEscapable(b byte) bool {
	switch b {
	case '\\', '"', '$', '`':
		return true
	default:
		return false
	}
}

func (t *interactiveArgTokenizer) consumeSingleQuoted(b byte) {
	if b == '\'' {
		t.quote = 0
		return
	}
	t.current.WriteByte(b)
}

func (t *interactiveArgTokenizer) consumeDoubleQuoted(b byte) {
	switch b {
	case '"':
		t.quote = 0
	case '\\':
		t.escaped = true
	default:
		t.current.WriteByte(b)
	}
}

func (t *interactiveArgTokenizer) consumeUnquoted(b byte) {
	switch b {
	case ' ', '\t':
		t.flush()
		return
	case '\'', '"':
		t.quote = b
	case '\\':
		t.escaped = true
	default:
		t.current.WriteByte(b)
	}
	// Opening a quote or an escape starts a token as well, so that `''` and
	// `""` yield an empty argument instead of nothing.
	t.started = true
}

// flush ends the current token, if one was started, empty or not.
func (t *interactiveArgTokenizer) flush() {
	if !t.started {
		return
	}
	t.tokens = append(t.tokens, t.current.String())
	t.current.Reset()
	t.started = false
}

func (t *interactiveArgTokenizer) finish() ([]string, error) {
	// An open quote is reported first: a backslash right before the end of
	// an open double-quoted string is part of that unterminated string.
	if t.quote != 0 {
		return nil, errors.New("unterminated quoted string")
	}
	if t.escaped {
		return nil, errors.New("unterminated escape sequence")
	}
	t.flush()
	return t.tokens, nil
}
