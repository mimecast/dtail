package clients

import (
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/mapr"
)

// queryTokenStyle returns the theme style of a MapReduce query token kind
// (mapr.AnalyzePartial), the styles of the matching Tab candidate kinds,
// so a word looks the same in a candidate list and in a query. Unknown
// tokens (words the parser rejects) stay unpainted.
func queryTokenStyle(theme config.InteractiveTermColors, kind mapr.TokenKind) config.TermStyle {
	switch kind {
	case mapr.TokenKeyword, mapr.TokenLogFormat:
		return theme.CandidateKeyword
	case mapr.TokenAggregation, mapr.TokenFunction:
		return theme.CandidateFunction
	case mapr.TokenField:
		return theme.CandidateField
	case mapr.TokenVariable:
		return theme.CandidateVariable
	case mapr.TokenOperator:
		return theme.CandidateOperator
	case mapr.TokenString, mapr.TokenNumber:
		return theme.CandidateValue
	case mapr.TokenTable:
		return theme.CandidateTable
	default:
		return config.TermStyle{}
	}
}

// queryHighlighter paints a MapReduce query by the token kinds of
// mapr.AnalyzePartial, with the parser's own tokenizer and clause rules, so
// what it paints is what the parser reads. The text between tokens
// (blanks, commas, parentheses) is kept as it is. Once the color sequences
// are stripped, the output equals the input (or its Go quoting, see
// quoted).
type queryHighlighter struct {
	theme config.InteractiveTermColors
	// base is the SGR sequence of the text around the tokens, set again
	// after every token; empty for the terminal's default.
	base string
	// under is the part of base a token keeps: its colors (foreground,
	// background), not its attribute, so a dim base does not dim the
	// tokens. Empty for the terminal's default.
	under string
}

// paint returns query highlighted, each segment passed through write
// first (the identity, or the Go quoting of quoted).
func (h queryHighlighter) paint(query string, write func(string) string) string {
	var sb strings.Builder
	sb.WriteString(h.base)
	pos := 0
	for _, token := range mapr.AnalyzePartial(query).Tokens {
		if token.Start < pos || token.End > len(query) || token.Start > token.End {
			continue // defensive: never repeat or drop a byte of the query
		}
		sb.WriteString(write(query[pos:token.Start]))
		text := write(query[token.Start:token.End])
		style := queryTokenStyle(h.theme, token.Kind)
		if style.Fg == "" && style.Attr == "" {
			sb.WriteString(text)
		} else {
			sb.WriteString(h.tokenStart() + string(style.Fg) + string(style.Attr) + text +
				string(color.AttrReset) + string(color.FgDefault) + h.base)
		}
		pos = token.End
	}
	sb.WriteString(write(query[pos:]))
	if h.base != "" {
		sb.WriteString(string(color.AttrReset) + string(color.BgDefault) + string(color.FgDefault))
	}
	return sb.String()
}

// tokenStart returns what precedes a painted token: with a base, a reset of
// its attribute and its colors again (under), the token's style on top.
func (h queryHighlighter) tokenStart() string {
	if h.base == "" {
		return ""
	}
	return string(color.AttrReset) + h.under
}

// plain returns query highlighted as it is.
func (h queryHighlighter) plain(query string) string {
	return h.paint(query, func(s string) string { return s })
}

// quoted returns the Go quoting of query (strconv.Quote, as :show prints
// it), highlighted. The tokenizer splits only where decoding the query as
// UTF-8 has a boundary (also around invalid bytes), so quoting each segment
// on its own yields the quoting of the whole query
// (FuzzQueryHighlightKeepsText).
func (h queryHighlighter) quoted(query string) string {
	return `"` + h.paint(query, func(s string) string {
		q := strconv.Quote(s)
		return q[1 : len(q)-1]
	}) + `"`
}

// query returns query highlighted with the interactive theme, or unchanged
// for a nil style.
func (s *interactiveStyle) query(query string) string {
	if s == nil {
		return query
	}
	return queryHighlighter{theme: s.theme}.plain(query)
}

// quotedQuery returns the Go quoting of query, highlighted with the
// interactive theme, or just quoted for a nil style.
func (s *interactiveStyle) quotedQuery(query string) string {
	if s == nil {
		return strconv.Quote(query)
	}
	return queryHighlighter{theme: s.theme}.quoted(query)
}
