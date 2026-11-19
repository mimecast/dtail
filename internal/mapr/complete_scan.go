package mapr

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// scannedToken is a token of tokenize together with the byte offsets of its
// source text in the query: query[start:end] is the bareword, or the quoted
// string including its double quotes.
type scannedToken struct {
	token
	start int
	end   int
	// open marks a double-quoted token whose closing quote is missing, so
	// that it runs to the end of the query.
	open bool
}

// scanQuery splits a query into the tokens of tokenize, with the same rules,
// and records where each token is in the query. tokenize splits at double
// quotes first (no escaping: every odd part is one quoted token, empty ones
// and an unterminated last one included) and the other parts at blanks
// (unicode.IsSpace, as strings.Fields) and commas. scanQuery walks the query
// once instead, which yields the same tokens in the same order; a unit test
// and a fuzz test compare both, so the two cannot drift apart. tokenize is
// kept as the parser's own, offset-free implementation.
func scanQuery(query string) []scannedToken {
	var tokens []scannedToken
	for i := 0; i < len(query); {
		r, size := utf8.DecodeRuneInString(query[i:])
		switch {
		case r == '"':
			st := scanQuoted(query, i)
			tokens = append(tokens, st)
			i = st.end
		case isTokenSeparator(r):
			i += size
		default:
			st := scanBareword(query, i)
			tokens = append(tokens, st)
			i = st.end
		}
	}
	return tokens
}

// scanQuoted scans the double-quoted token whose opening quote is at start.
func scanQuoted(query string, start int) scannedToken {
	rest := query[start+1:]
	closing := strings.IndexByte(rest, '"')
	if closing < 0 {
		return scannedToken{token: token{str: rest}, start: start, end: len(query), open: true}
	}
	return scannedToken{
		token: token{str: rest[:closing]},
		start: start,
		end:   start + 1 + closing + 1,
	}
}

// scanBareword scans the bareword starting at start. It ends before the next
// blank, comma or double quote, or at the end of the query.
func scanBareword(query string, start int) scannedToken {
	end := start
	for end < len(query) {
		r, size := utf8.DecodeRuneInString(query[end:])
		if r == '"' || isTokenSeparator(r) {
			break
		}
		end += size
	}
	return scannedToken{
		token: token{str: query[start:end], isBareword: true},
		start: start,
		end:   end,
	}
}

// isTokenSeparator tells whether r separates barewords: tokenize replaces
// commas with blanks and splits with strings.Fields, which splits at
// unicode.IsSpace. An invalid UTF-8 byte decodes to utf8.RuneError in both,
// which is no space.
func isTokenSeparator(r rune) bool {
	return r == ',' || unicode.IsSpace(r)
}
