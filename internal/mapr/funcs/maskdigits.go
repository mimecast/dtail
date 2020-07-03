package funcs

// MaskDigits masks all digits (replaces them with .)
func MaskDigits(input string) string {
	s := []byte(input)

	for i, b := range s {
		if '0' <= b && b <= '9' {
			s[i] = '.'
		}
	}

	return string(s)
}
