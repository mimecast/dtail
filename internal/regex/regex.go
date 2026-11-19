package regex

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
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
	isLiteral    bool   // true if pattern contains no regex metacharacters
	literalStr   string // literal string for string matching
	literalBytes []byte // literal bytes for byte matching
}

// String returns the string representation of Regex.
func (r Regex) String() string {
	return fmt.Sprintf("Regex(regexStr:%s,flags:%s,initialized:%t,re==nil:%t,isLiteral:%t)",
		r.regexStr, r.flags, r.initialized, r.re == nil, r.isLiteral)
}

// isLiteralPattern checks if the pattern contains no regex metacharacters.
// It returns true only for patterns that can be matched using simple string contains.
func isLiteralPattern(pattern string) bool {
	// Check for common regex metacharacters
	// Note: We're being conservative here - only treating truly literal strings as literals
	metaChars := `.+*?^$[]{}()|\\`
	for _, ch := range pattern {
		if strings.ContainsRune(metaChars, ch) {
			return false
		}
	}
	return true
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

	// Check if this is a literal pattern for optimization
	if isLiteralPattern(regexStr) {
		r.isLiteral = true
		r.literalStr = regexStr
		r.literalBytes = []byte(regexStr)
		r.initialized = true
		// We still compile the regex for backward compatibility and as a fallback
		// This ensures serialization/deserialization works correctly
		re, err := regexp.Compile(regexStr)
		if err != nil {
			return r, err
		}
		r.re = re
		return r, nil
	}

	// For non-literal patterns, compile as regex
	re, err := regexp.Compile(regexStr)
	if err != nil {
		return r, err
	}

	r.re = re
	r.initialized = true
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
func (r Regex) Serialize() (string, error) {
	var flags []string
	for _, flag := range r.flags {
		flags = append(flags, flag.String())
	}
	if !r.initialized {
		return "", fmt.Errorf("unable to serialize regex as not initialized properly: %v", r)
	}
	// Include literal flag in serialization if applicable
	if r.isLiteral {
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
	forceLiteral := false
	if strings.Contains(flagsStr, ":") {
		s := strings.SplitN(flagsStr, ":", 2)
		for _, flagStr := range strings.Split(s[1], ",") {
			if flagStr == "literal" {
				// This is our optimization hint, not a regular flag
				forceLiteral = true
				continue
			}
			flag, err := NewFlag(flagStr)
			if err != nil {
				return Regex{}, fmt.Errorf("unknown regex flag %q: %w", flagStr, err)
			}
			flags = append(flags, flag)
		}
	}

	// Create the regex with proper literal detection
	r, err := newRegex(regexStr, flags)
	if err != nil {
		return r, err
	}

	// If the serialized form indicated it was literal, ensure we treat it as such
	// This maintains consistency across client-server communication
	if forceLiteral && !r.isLiteral {
		// The pattern might have been literal on the client but not detected as such here
		// This could happen if our isLiteralPattern logic changes
		// For safety, we'll trust the serialized hint
		r.isLiteral = true
		r.literalStr = regexStr
		r.literalBytes = []byte(regexStr)
	}

	return r, nil
}
