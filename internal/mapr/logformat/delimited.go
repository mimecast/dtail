package logformat

import "strings"

func scanDelimitedField(input string, start int, delimiter byte) (token string, next int, done bool) {
	index := strings.IndexByte(input[start:], delimiter)
	if index < 0 {
		return input[start:], len(input), true
	}
	end := start + index
	return input[start:end], end + 1, false
}
