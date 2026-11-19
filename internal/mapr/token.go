package mapr

import (
	"slices"
	"strings"
)

// keywords are the clause keywords of the query language, in lower case. A
// bareword equal to one of them (ignoring case) starts a new clause, so the
// clauses can come in any order (see doc/querylanguage.md). The tokenizer
// (token.isKeyword) and Keywords, which the interactive query completer
// reads, share this one table so that they cannot drift apart. A new clause
// needs an entry here and a case in Query.parseTokens.
var keywords = [...]string{"select", "from", "where", "set", "group", "rorder",
	"order", "interval", "limit", "outfile", "logformat"}

// Represents a parsed token, used to parse the mapr query.
type token struct {
	str            string
	isBareword     bool
	quotesStripped bool
}

// Keywords returns the clause keywords of the query language in lower case,
// the words a clause starts with, in the order of the parser's table. The
// result is a copy, which the caller may change.
func Keywords() []string {
	return slices.Clone(keywords[:])
}

// tokenize parses a query string into tokens. scanQuery (complete_scan.go)
// splits the same way and records offsets for AnalyzePartial; a change of
// the rules here must be made there too, which its tests enforce.
func tokenize(queryStr string) []token {
	var tokens []token
	for i, part := range strings.Split(queryStr, "\"") {
		// Even i, means that it is not a quoted string
		if i%2 == 0 {
			commasStripped := strings.ReplaceAll(part, ",", " ")
			for _, tokenStr := range strings.Fields(commasStripped) {
				parsedToken := token{
					str:        tokenStr,
					isBareword: true,
				}
				tokens = append(tokens, parsedToken)
			}
			continue
		}
		// Add whole quoted string as a token
		parsedToken := token{
			str:        part,
			isBareword: false,
		}
		tokens = append(tokens, parsedToken)
	}
	return tokens
}

func tokensConsume(tokens []token) ([]token, []token) {
	var consumed []token
	for i, current := range tokens {
		if current.isKeyword() {
			return tokens[i:], consumed
		}
		consumed = append(consumed, stripBackticks(current))
	}
	return nil, consumed
}

// stripBackticks strips the escapes of a token that is enclosed in backticks
// as a whole, such as `foo`, and marks it quotesStripped. This allows to use
// keywords and aggregation-like names as field names. A backtick anywhere
// else (`foo, fo`o) is part of the token. The parser (tokensConsume) and
// AnalyzePartial share this rule.
func stripBackticks(t token) token {
	length := len(t.str)
	if length >= 2 && t.str[0] == '`' && t.str[length-1] == '`' {
		return token{str: t.str[1 : length-1], isBareword: t.isBareword, quotesStripped: true}
	}
	return t
}

func tokensConsumeOptional(tokens []token, optional string) []token {
	if len(tokens) < 1 {
		return tokens
	}
	// if strings.ToLower(tokens[0].str) == strings.ToLower(optional) {
	if strings.EqualFold(tokens[0].str, optional) {
		return tokens[1:]
	}
	return tokens
}

func (t token) isKeyword() bool {
	if !t.isBareword {
		return false
	}
	for _, keyword := range keywords {
		if strings.ToLower(t.str) == keyword {
			return true
		}
	}
	return false
}

func (t token) String() string {
	return t.str
}
