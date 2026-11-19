package clients

import (
	"slices"
	"strings"
	"unicode"

	"github.com/mimecast/dtail/internal/mapr"
)

// This file writes names (fields, tables, select items) as a word of a
// query, so that the query tokenizer and parser read the word as that name:
// the tokenizer splits barewords at blanks and commas and has no escape for
// a double quote; a bareword equal to a clause keyword starts a clause; the
// first word of group, order and rorder is dropped when it is by; and the
// parser strips one pair of backticks around a whole token, quoted or not
// (doc/querylanguage.md).

// queryFieldWords returns names as they are written as a field of a query:
// a name the query tokenizer would split or read as something else (a
// blank of unicode.IsSpace, a comma, quote, backtick or parenthesis in it)
// is left out, as no quoting writes it as a field everywhere (inside a
// double quote quotedQueryWords writes such fields); a name that is a clause keyword
// (a field named from, order, ...) or by (the optional first word of group
// by) is written in backticks, which the parser strips, unless the word is
// already inside backticks. Names with a control or format character are
// dropped later for every candidate kind (queryCandidateSource.groups).
func queryFieldWords(names []string, inBacktick bool) []string {
	var words []string
	for _, name := range names {
		switch {
		case name == "" || hasQuerySeparator(name) || strings.ContainsAny(name, "\"`()'"):
		case !inBacktick && isReservedQueryWord(name):
			words = append(words, "`"+name+"`")
		default:
			words = append(words, name)
		}
	}
	return words
}

// quotedQueryWords returns names as they are written inside an open double
// quote, each closed by the quote: as they are, but in backticks where the
// parser would read the quoted token as something else (quotedQueryWord).
// A name holding a double quote can not be written there and is left out.
// fields tells that the names are select or group fields, where a quoted
// name with a parenthesis would be read as an aggregation.
func quotedQueryWords(names []string, fields bool) []string {
	var words []string
	for _, name := range names {
		if word, ok := quotedQueryWord(name, fields); ok {
			words = append(words, word)
		}
	}
	return words
}

// querySelectItemWords returns the select items (Partial.SelectItems, as
// order by compares them) as they are written after order by, followed by
// a blank, for a word in quote: "" (a bareword), a double quote or a
// backtick (the word is inside one, which the candidate closes). A bareword
// item that the tokenizer would split is double-quoted, a keyword or by in
// backticks (count(x) stays as is: order by compares the token, it parses
// no aggregation). An item that can not be written in quote is left out.
func querySelectItemWords(items []string, quote string) []string {
	var words []string
	for _, item := range items {
		if word, ok := selectItemWord(item, quote); ok {
			words = append(words, word+" ")
		}
	}
	return words
}

// selectItemWord writes one select item for querySelectItemWords.
func selectItemWord(item, quote string) (string, bool) {
	if item == "" || strings.Contains(item, `"`) {
		return "", false
	}
	switch quote {
	case `"`:
		return quotedQueryWord(item, false)
	case "`":
		// The whole token, `item`, is stripped of its backticks.
		if hasQuerySeparator(item) {
			return "", false
		}
		return item + "`", true
	}
	switch {
	case hasQuerySeparator(item):
		word, _ := quotedQueryWord(item, false)
		return `"` + word, true
	case isReservedQueryWord(item) || isBacktickWrapped(item):
		return "`" + item + "`", true
	}
	return item, true
}

// quotedQueryWord writes name inside an open double quote and closes it. A
// quoted token is no keyword, but the parser still strips a pair of
// backticks around it and drops a first by of group or order, so a name
// wrapped in backticks or equal to by gets another pair of backticks, as
// does a field (fields) with a parenthesis, which a quoted select item
// would otherwise be parsed as an aggregation of. A name with a double
// quote can not be written.
func quotedQueryWord(name string, fields bool) (string, bool) {
	if name == "" || strings.Contains(name, `"`) {
		return "", false
	}
	if isBacktickWrapped(name) || strings.EqualFold(name, "by") ||
		(fields && strings.ContainsAny(name, "()")) {
		name = "`" + name + "`"
	}
	return name + `"`, true
}

// isReservedQueryWord tells whether a bareword name would be read as a
// clause keyword or as the optional by of group, order and rorder.
func isReservedQueryWord(name string) bool {
	lower := strings.ToLower(name)
	return lower == "by" || slices.Contains(mapr.Keywords(), lower)
}

// isBacktickWrapped tells whether the parser would strip backticks from a
// token name (mapr's stripBackticks).
func isBacktickWrapped(name string) bool {
	return len(name) >= 2 && name[0] == '`' && name[len(name)-1] == '`'
}

// hasQuerySeparator tells whether the tokenizer would split a bareword
// name: at a comma or a blank (unicode.IsSpace, as strings.Fields).
func hasQuerySeparator(name string) bool {
	return strings.ContainsFunc(name, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}
