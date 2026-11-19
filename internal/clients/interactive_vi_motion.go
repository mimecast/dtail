package clients

import "unicode"

// Motions of the vi editing mode (viEditor) on the line as runes. Each
// returns the target position and whether the motion includes the
// character at the target when an operator (d, c, y) applies it, as in
// vi: e, E, f, t and $ include it, the others do not.

// viCharClass is the class of r for word motions: 0 blank, 1 word
// character (letter, digit, underscore), 2 other. With big (W, B, E) every
// non-blank is one class, as a WORD is any run of non-blanks.
func viCharClass(r rune, big bool) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case big || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return 1
	default:
		return 2
	}
}

// viNextWordStart returns the start of the word after the one at pos, or
// len(line) when there is none (so dw on the last word deletes to the end).
func viNextWordStart(line []rune, pos int, big bool) int {
	n := len(line)
	if pos >= n {
		return n
	}
	i := pos
	if class := viCharClass(line[i], big); class != 0 {
		for i < n && viCharClass(line[i], big) == class {
			i++
		}
	}
	for i < n && viCharClass(line[i], big) == 0 {
		i++
	}
	return i
}

// viPrevWordStart returns the start of the word before pos (or of the word
// pos is in, when pos is not at its start), or 0.
func viPrevWordStart(line []rune, pos int, big bool) int {
	i := pos - 1
	for i >= 0 && viCharClass(line[i], big) == 0 {
		i--
	}
	if i < 0 {
		return 0
	}
	class := viCharClass(line[i], big)
	for i > 0 && viCharClass(line[i-1], big) == class {
		i--
	}
	return i
}

// viWordEnd returns the last character of the word after pos (of the word
// pos is in, when pos is not at its end), or pos when only blanks follow.
func viWordEnd(line []rune, pos int, big bool) int {
	n := len(line)
	i := pos + 1
	for i < n && viCharClass(line[i], big) == 0 {
		i++
	}
	if i >= n {
		// Only blanks follow: e does not move, as in vi.
		return min(pos, max(n-1, 0))
	}
	class := viCharClass(line[i], big)
	for i+1 < n && viCharClass(line[i+1], big) == class {
		i++
	}
	return i
}

// viFirstNonBlank returns the position of the line's first non-blank, or
// its end.
func viFirstNonBlank(line []rune) int {
	for i, r := range line {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return len(line)
}

// viFind returns the position f, F, t or T (kind) with count moves to for
// char, and false when the line has fewer than count such characters in
// that direction. t and T stop next to the character found.
func viFind(line []rune, pos int, kind, char rune, count int) (int, bool) {
	step := 1
	if kind == 'F' || kind == 'T' {
		step = -1
	}
	i := pos
	for found := 0; found < count; {
		i += step
		if i < 0 || i >= len(line) {
			return pos, false
		}
		if line[i] == char {
			found++
		}
	}
	switch kind {
	case 't':
		return i - 1, true
	case 'T':
		return i + 1, true
	}
	return i, true
}

// viMotion returns where motion key with count moves from pos, whether an
// operator includes the target character, and false for no motion key or
// a motion that finds nothing. char is the character of f, F, t and T.
func viMotion(line []rune, pos int, key, char rune, count int) (int, bool, bool) {
	n := len(line)
	target := pos
	switch key {
	case 'h':
		return max(pos-count, 0), false, true
	case 'l', ' ':
		return min(pos+count, n), false, true
	case '0':
		return 0, false, true
	case '^':
		return viFirstNonBlank(line), false, true
	case '$':
		return n, false, true
	case 'w', 'W':
		for i := 0; i < count; i++ {
			target = viNextWordStart(line, target, key == 'W')
		}
		return target, false, true
	case 'b', 'B':
		for i := 0; i < count; i++ {
			target = viPrevWordStart(line, target, key == 'B')
		}
		return target, false, true
	case 'e', 'E':
		for i := 0; i < count; i++ {
			target = viWordEnd(line, target, key == 'E')
		}
		return target, true, true
	case 'f', 'F', 't', 'T':
		target, ok := viFind(line, pos, key, char, count)
		return target, key == 'f' || key == 't', ok
	}
	return pos, false, false
}
