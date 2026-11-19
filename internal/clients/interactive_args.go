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

// partialArgs is the result of tokenizing a :reload line typed up to the
// cursor (splitInteractiveArgsPartial): the finished tokens and the token
// the cursor is in, which may be inside an open quote.
type partialArgs struct {
	// tokens are the finished tokens before the current one.
	tokens []string
	// current is the decoded current token so far ("" when none started).
	current string
	// started is true when a current token began (also an empty quoted one).
	started bool
	// quote is the open quote of the current token, 0 outside quotes.
	quote byte
	// escaped is true when the line ends in an escaping backslash.
	escaped bool
	// rawStarts maps every byte of current to the offset in the raw line
	// where its encoding starts (the backslash of an escape), so that a
	// completion can replace a decoded suffix in the raw line.
	rawStarts []int
	// quotes holds, for every byte of current, the quote it was read in (0
	// outside quotes): a suffix whose bytes were all read in the current
	// quote can be replaced by text escaped for that quote.
	quotes []byte
}

// suffixInQuote reports whether the decoded bytes of current from i on were
// all read inside the open quote of the current token, so that its raw
// text can be replaced by text escaped for that quote.
func (a partialArgs) suffixInQuote(i int) bool {
	for _, q := range a.quotes[i:] {
		if q != a.quote {
			return false
		}
	}
	return true
}

// rawOffset returns the offset in the raw line where the decoded byte i of
// the current token starts, or rawLen for i == len(current).
func (a partialArgs) rawOffset(i, rawLen int) int {
	if i >= len(a.rawStarts) {
		return rawLen
	}
	return a.rawStarts[i]
}

// splitInteractiveArgsPartial tokenizes raw like splitInteractiveArgs, but
// for a line still being typed: an open quote or a trailing backslash is no
// error but reported for the current token, whose decoded bytes are mapped
// back to their raw offsets. It uses the same consume methods as the strict
// tokenizer, so both split alike.
func splitInteractiveArgsPartial(raw string) partialArgs {
	var t interactiveArgTokenizer
	var rawStarts []int
	var quotes []byte
	escapeStart := -1
	for i := 0; i < len(raw); i++ {
		before := t.current.Len()
		tokensBefore := len(t.tokens)
		quoteBefore := t.quote
		if !t.escaped {
			escapeStart = i
		}
		t.consume(raw[i])
		if len(t.tokens) != tokensBefore {
			rawStarts = rawStarts[:0]
			quotes = quotes[:0]
		}
		// Every byte written for raw[i] starts where its encoding started:
		// at the backslash for an escaped byte (also the kept backslash of
		// an escape inside double quotes that escapes nothing).
		for n := before; n < t.current.Len(); n++ {
			rawStarts = append(rawStarts, escapeStart)
			quotes = append(quotes, quoteBefore)
		}
	}
	return partialArgs{
		tokens:    t.tokens,
		current:   t.current.String(),
		started:   t.started,
		quote:     t.quote,
		escaped:   t.escaped,
		rawStarts: rawStarts,
		quotes:    quotes,
	}
}

// escapeForQuote encodes text so that splitInteractiveArgs decodes it back
// unchanged inside the quote context quote (0 outside quotes, otherwise the
// single or double quote byte),
// for a completion inserted into an open quoted value. A single quote
// inside single quotes closes them, adds a backslash-escaped quote and
// opens them again; inside double quotes the four escapable characters get
// a backslash; outside quotes every blank, quote and backslash does.
func escapeForQuote(text string, quote byte) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote == '\'' && c == '\'':
			b.WriteString(`'\''`)
			continue
		case quote == '"' && isDoubleQuoteEscapable(c):
			b.WriteByte('\\')
		case quote == 0 && strings.IndexByte(" \t'\"\\", c) >= 0:
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}
