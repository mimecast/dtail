package mapr

import (
	"strings"
)

var keywords = [...]string{"select", "from", "where", "set", "group", "rorder",
	"order", "interval", "limit", "outfile", "logformat"}

// Represents a parsed token, used to parse the mapr query.
type token struct {
	str            string
	isBareword     bool
	quotesStripped bool
}

// tokenize parses a query string into tokens.
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
		// strip escapes, such as ` from `foo`, this allows to use keywords as field names
		length := len(current.str)
		if length == 0 {
			continue
		}
		if length >= 2 && current.str[0] == '`' && current.str[length-1] == '`' {
			stripped := current.str[1 : length-1]
			normalized := token{
				str:            stripped,
				isBareword:     current.isBareword,
				quotesStripped: true,
			}
			consumed = append(consumed, normalized)
			continue
		}
		consumed = append(consumed, current)
	}
	return nil, consumed
}

func tokensConsumeStr(tokens []token) ([]token, []string) {
	var strings []string
	tokens, found := tokensConsume(tokens)
	for _, token := range found {
		strings = append(strings, token.str)
	}
	return tokens, strings
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
