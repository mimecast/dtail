package mapr

import (
	"strings"
)

var keywords = [...]string{"select", "from", "where", "set", "group", "rorder",
	"order", "interval", "limit", "outfile", "logformat"}

// Represents a parsed token, used to parse the mapr query.
type token struct {
	str        string
	isBareword bool
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

func tokenize(queryStr string) []token {
	var tokens []token
	for i, part := range strings.Split(queryStr, "\"") {
		// Even i, means that it is not a quoted string
		if i%2 == 0 {
			commasStripped := strings.Replace(part, ",", " ", -1)
			for _, tokenStr := range strings.Fields(commasStripped) {
				token := token{
					str:        strings.ToLower(tokenStr),
					isBareword: true,
				}
				tokens = append(tokens, token)
			}
			continue
		}
		// Add whole quoted string as a token
		token := token{
			str:        part,
			isBareword: false,
		}
		tokens = append(tokens, token)
	}
	return tokens
}

func tokensConsume(tokens []token) ([]token, []token) {
	//dlog.Common.Trace("=====================")
	var consumed []token
	for i, t := range tokens {
		if t.isKeyword() {
			//dlog.Common.Trace("keyword", t)
			return tokens[i:], consumed
		}
		// strip escapes, such as ` from `foo`, this allows to use keywords as field names
		length := len(t.str)
		if length == 0 {
			continue
		}
		if t.str[0] == '`' && t.str[length-1] == '`' {
			stripped := t.str[1 : length-1]
			//dlog.Common.Trace("stripped", stripped)
			t := token{
				str:        strings.ToLower(stripped),
				isBareword: t.isBareword,
			}
			consumed = append(consumed, t)
			continue
		}
		//dlog.Common.Trace("bare", token)
		consumed = append(consumed, t)
	}
	//dlog.Common.Trace("result", consumed)
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
	if strings.ToLower(tokens[0].str) == strings.ToLower(optional) {
		return tokens[1:]
	}
	return tokens
}
