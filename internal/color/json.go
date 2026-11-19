package color

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Config-file colour decoding never fails on a string value. A config file
// that loaded with an older release must keep loading, because dserver reads
// the same file and a fatal colour error would stop it even though it never
// renders colours. The accepted forms are, in order:
//
//   - A raw ANSI SGR escape string (one or more ESC[...m sequences, with ';' or
//     ':' separators) from older config files, used unchanged.
//   - A colour or attribute name, matched case-insensitively, optionally with
//     the legacy constant prefix that the released example config used
//     ("FgBlack", "BgCyan", "AttrDim"). Colour fields accept either colour
//     prefix, because the colour itself is unambiguous; attribute fields
//     accept only "Attr".
//   - The empty string, meaning no escape code at all (colours and attributes).
//
// Any other string keeps the field's current (default) value and records a
// warning, which the config loader reports on stderr (see TakeConfigWarnings).
// A JSON null also keeps the current value; a non-string value is an error.

// rawSGR matches one or more raw ANSI SGR escape sequences such as "\x1b[37m",
// "\x1b[31m\x1b[1m" or "\x1b[38:5:208m". Old config files stored colours this
// way because the decoder used to copy the configured string verbatim.
var rawSGR = regexp.MustCompile(`^(?:\x1b\[[0-9;:]*m)+$`)

var (
	warningsMu sync.Mutex
	warnings   []string
)

// TakeConfigWarnings returns the warnings recorded while decoding config-file
// colour values since the previous call, and clears them. Each warning names
// an unrecognised value that was replaced by the field's default.
func TakeConfigWarnings() []string {
	warningsMu.Lock()
	defer warningsMu.Unlock()
	taken := warnings
	warnings = nil
	return taken
}

func recordWarning(kind, value string, names []string) {
	warningsMu.Lock()
	defer warningsMu.Unlock()
	warnings = append(warnings, fmt.Sprintf(
		"ignoring invalid %s %q, using the default instead: must be one of %s (case-insensitive)",
		kind, value, strings.Join(names, ", ")))
}

// UnmarshalJSON decodes a foreground colour from a config file; see the comment
// at the top of this file for the accepted forms.
func (c *FgColor) UnmarshalJSON(data []byte) error {
	sp, err := decodeConfigString(data, "foreground color")
	if err != nil || sp == nil {
		return err
	}
	s := *sp
	switch {
	case s == "" || rawSGR.MatchString(s):
		*c = FgColor(s)
	default:
		fg, err := ToFgColor(stripPrefix(s, "fg", "bg"))
		if err != nil {
			recordWarning("foreground color", s, ColorNames)
			return nil
		}
		*c = fg
	}
	return nil
}

// UnmarshalJSON decodes a background colour from a config file; see the comment
// at the top of this file for the accepted forms.
func (c *BgColor) UnmarshalJSON(data []byte) error {
	sp, err := decodeConfigString(data, "background color")
	if err != nil || sp == nil {
		return err
	}
	s := *sp
	switch {
	case s == "" || rawSGR.MatchString(s):
		*c = BgColor(s)
	default:
		bg, err := ToBgColor(stripPrefix(s, "bg", "fg"))
		if err != nil {
			recordWarning("background color", s, ColorNames)
			return nil
		}
		*c = bg
	}
	return nil
}

// UnmarshalJSON decodes a text attribute from a config file; see the comment
// at the top of this file for the accepted forms.
func (a *Attribute) UnmarshalJSON(data []byte) error {
	sp, err := decodeConfigString(data, "text attribute")
	if err != nil || sp == nil {
		return err
	}
	s := *sp
	if rawSGR.MatchString(s) {
		*a = Attribute(s)
		return nil
	}
	attr, err := ToAttribute(stripPrefix(s, "attr"))
	if err != nil {
		recordWarning("text attribute", s, AttributeNames)
		return nil
	}
	*a = attr
	return nil
}

// stripPrefix removes the first of the given lower-case prefixes that s starts
// with, ignoring case. Only a prefix followed by more text is removed, so a
// bare prefix is left for the name lookup to reject.
func stripPrefix(s string, prefixes ...string) string {
	lower := strings.ToLower(s)
	for _, prefix := range prefixes {
		if len(s) > len(prefix) && strings.HasPrefix(lower, prefix) {
			return s[len(prefix):]
		}
	}
	return s
}

// decodeConfigString decodes a JSON string. It returns nil for a JSON null so
// the caller keeps its current (default) value, like encoding/json does for
// plain string fields.
func decodeConfigString(data []byte, kind string) (*string, error) {
	var s *string
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid %s %s: must be a string: %w", kind, data, err)
	}
	return s, nil
}
