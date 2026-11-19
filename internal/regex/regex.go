package regex

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Regex for filtering lines.
type Regex struct {
	// The original regex string
	regexStr string
	// The Golang regexp object
	re *regexp.Regexp
	// For now only use the first flag at flags[0], but in the future we can
	// set and use multiple flags.
	flags       []Flag
	initialized bool
	// Fields for optimized literal string matching
	isLiteral    bool   // true if the pattern is equivalent to a substring search
	literalStr   string // literal string for string matching
	literalBytes []byte // literal bytes for byte matching
}

// String returns the string representation of Regex.
func (r Regex) String() string {
	return fmt.Sprintf("Regex(regexStr:%s,flags:%s,initialized:%t,re==nil:%t,isLiteral:%t)",
		r.regexStr, r.flags, r.initialized, r.re == nil, r.isLiteral)
}

// metaChars are the characters which, unescaped, give a pattern a meaning
// beyond a plain substring search. The backslash is included: it starts an
// escape sequence, which literalPattern handles separately.
const metaChars = `.+*?^$[]{}()|\`

// isLiteralEscape reports whether the two byte sequence `\`+c matches exactly
// the single character c in Go's regexp syntax.
//
// Go's parser (regexp/syntax) treats an escaped ASCII character as a literal
// when the character is not alphanumeric, and gives every escape of an
// alphanumeric character a special meaning instead: character classes (\d \w
// \s \D \W \S \p{L}), zero width assertions (\b \B \A \z), literal text spans
// (\Q \E), C style escapes (\a \f \n \r \t \v) and numeric escapes (\x41,
// \0, \1 to \7). Rather than relying on that rule, the accepted escapes are
// listed here one by one: it is the complete set of ASCII punctuation and is
// pinned against regexp itself by TestLiteralEscapeAgreesWithRegexp. Anything
// not listed (alphanumeric escapes, escaped spaces, escaped non-ASCII runes)
// makes literalPattern give up, so the compiled regexp is used instead.
func isLiteralEscape(c byte) bool {
	switch c {
	case '!', '"', '#', '$', '%', '&', '\'', '(', ')', '*', '+', ',', '-',
		'.', '/', ':', ';', '<', '=', '>', '?', '@', '[', '\\', ']', '^',
		'_', '`', '{', '|', '}', '~':
		return true
	default:
		return false
	}
}

// literalPattern reports whether the pattern matches exactly the same input as
// a search for a plain substring, and returns that substring.
//
// Patterns without metacharacters are their own literal. Patterns whose only
// metacharacters are backslash-escaped punctuation, such as the dmap line
// filter `\|MAPREDUCE:STATS\|`, are the literal obtained by dropping those
// backslashes. Everything else, including any escape with a special meaning,
// is rejected so that the caller falls back to the compiled regexp.
func literalPattern(pattern string) (string, bool) {
	// Patterns without a backslash need no unescaping and can be returned as
	// they are, without building a second copy of the string.
	unescape := strings.IndexByte(pattern, '\\') >= 0

	var literal strings.Builder
	if unescape {
		literal.Grow(len(pattern))
	}

	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch {
		case c == '\\':
			// A trailing backslash does not compile at all; an escape which
			// is not plain punctuation may mean anything but itself.
			if i+1 == len(pattern) || !isLiteralEscape(pattern[i+1]) {
				return "", false
			}
			literal.WriteByte(pattern[i+1])
			i += 2

		case c < utf8.RuneSelf:
			if strings.IndexByte(metaChars, c) >= 0 {
				return "", false
			}
			if unescape {
				literal.WriteByte(c)
			}
			i++

		default:
			// Both cases which make DecodeRuneInString return RuneError are
			// left to regexp, for different reasons. A pattern holding
			// invalid UTF-8 does not compile at all ("error parsing regexp:
			// invalid UTF-8"), so newRegex must report that error rather
			// than match anything. A pattern holding a validly encoded
			// U+FFFD does compile, but regexp maps every decoding error in
			// the input to U+FFFD as well, so it matches invalid bytes which
			// a plain byte search does not find.
			r, size := utf8.DecodeRuneInString(pattern[i:])
			if r == utf8.RuneError {
				return "", false
			}
			if unescape {
				literal.WriteString(pattern[i : i+size])
			}
			i += size
		}
	}

	if !unescape {
		return pattern, true
	}
	return literal.String(), true
}

// NewNoop is a noop regex (doing nothing).
func NewNoop() Regex {
	return Regex{
		flags:       []Flag{Noop},
		initialized: true,
	}
}

// New returns a new regex object.
func New(regexStr string, flag Flag) (Regex, error) {
	if regexStr == "" || regexStr == "." || regexStr == ".*" {
		return NewNoop(), nil
	}
	return newRegex(regexStr, []Flag{flag})
}

func newRegex(regexStr string, flags []Flag) (Regex, error) {
	if len(flags) == 0 {
		flags = append(flags, Default)
	}

	r := Regex{
		regexStr: regexStr,
		flags:    flags,
	}

	// The regex is compiled in either case: it is the fallback for patterns
	// which are not literals, and compiling a literal pattern too keeps
	// invalid patterns an error no matter how they match.
	re, err := regexp.Compile(regexStr)
	if err != nil {
		return r, err
	}
	r.re = re
	r.initialized = true

	// Check if this is a literal pattern for optimization.
	if literalStr, ok := literalPattern(regexStr); ok {
		r.isLiteral = true
		r.literalStr = literalStr
		r.literalBytes = []byte(literalStr)
	}

	return r, nil
}

// Match a byte string.
func (r Regex) Match(b []byte) bool {
	if len(r.flags) == 0 {
		return true
	}

	// Use optimized literal matching if possible
	if r.isLiteral {
		switch r.flags[0] {
		case Default:
			return bytes.Contains(b, r.literalBytes)
		case Invert:
			return !bytes.Contains(b, r.literalBytes)
		case Noop:
			return true
		default:
			return false
		}
	}

	// Fall back to regex matching for non-literal patterns
	switch r.flags[0] {
	case Default:
		return r.re.Match(b)
	case Invert:
		return !r.re.Match(b)
	case Noop:
		return true
	default:
		return false
	}
}

// MatchString matches a string.
func (r Regex) MatchString(str string) bool {
	if len(r.flags) == 0 {
		return true
	}

	// Use optimized literal matching if possible
	if r.isLiteral {
		switch r.flags[0] {
		case Default:
			return strings.Contains(str, r.literalStr)
		case Invert:
			return !strings.Contains(str, r.literalStr)
		case Noop:
			return true
		default:
			return false
		}
	}

	// Fall back to regex matching for non-literal patterns
	switch r.flags[0] {
	case Default:
		return r.re.MatchString(str)
	case Invert:
		return !r.re.MatchString(str)
	case Noop:
		return true
	default:
		return false
	}
}

// Serialize the regex.
//
// Serialize only emits what Deserialize accepts: a regex which is not
// initialized, which has no flag, or which carries a flag other than Default,
// Invert or Noop (such as Undefined) is an error rather than a wire string the
// receiving server would reject.
func (r Regex) Serialize() (string, error) {
	if !r.initialized {
		return "", fmt.Errorf("unable to serialize regex as not initialized properly: %v", r)
	}
	if len(r.flags) == 0 {
		return "", fmt.Errorf("unable to serialize regex without a flag: %v", r)
	}
	flags := make([]string, 0, len(r.flags)+1)
	for _, flag := range r.flags {
		if !flag.serializable() {
			return "", fmt.Errorf("unable to serialize regex with flag %q: %v", flag, r)
		}
		flags = append(flags, flag.String())
	}
	// Include the literal hint in the serialization, but only for patterns
	// which are their own literal. The receiver derives the literal from the
	// pattern itself, so the hint is redundant among peers of this version;
	// older peers however take the hint as permission to search for the
	// pattern text verbatim, which is only the same search when the pattern
	// needs no unescaping.
	if r.isLiteral && r.literalStr == r.regexStr {
		flags = append(flags, "literal")
	}
	return fmt.Sprintf("regex:%s %s", strings.Join(flags, ","), r.regexStr), nil
}

// IsLiteral returns true if this regex is using literal string matching
func (r Regex) IsLiteral() bool {
	return r.isLiteral
}

// Pattern returns the original pattern string
func (r Regex) Pattern() string {
	return r.regexStr
}

// Deserialize the regex.
func Deserialize(str string) (Regex, error) {
	// Get regex string
	s := strings.SplitN(str, " ", 2)
	if len(s) < 2 {
		return NewNoop(), nil
	}
	flagsStr := s[0]
	regexStr := s[1]

	if !strings.HasPrefix(flagsStr, "regex") {
		return Regex{}, fmt.Errorf("unable to deserialize regex '%s': should start "+
			"with string 'regex'", str)
	}

	// Parse regex flags, e.g. "regex:flag1,flag2,flag3..."
	var flags []Flag
	if strings.Contains(flagsStr, ":") {
		s := strings.SplitN(flagsStr, ":", 2)
		for _, flagStr := range strings.Split(s[1], ",") {
			if flagStr == "literal" {
				// An optimization hint of the sending side, not a flag. It is
				// accepted for compatibility but never trusted: the literal is
				// derived from the pattern below, and a pattern which cannot
				// be proven to be a literal keeps the compiled regexp, which
				// matches whatever the sender's regexp matched.
				continue
			}
			flag, err := NewFlag(flagStr)
			if err != nil {
				return Regex{}, fmt.Errorf("unknown regex flag %q: %w", flagStr, err)
			}
			flags = append(flags, flag)
		}
	}

	return newRegex(regexStr, flags)
}
