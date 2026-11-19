package protocol

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// writeEscapedDetail appends one diagnostic detail to dst with every control
// and format character escaped.
//
// Details carry text a client chose (a requested path, a glob, a probe
// entry) and text a server sent (relayed errors). Written verbatim, a newline
// in it would start a forged line in the log file and on the terminal, and an
// ESC sequence or a bidi override would rewrite what the terminal shows. So
// C0 controls (tab included), DEL, C1 controls, format characters (Cf, e.g.
// U+202E) and the line and paragraph separators U+2028/U+2029 are written as
// Go escapes (\n, \r, \t, \xNN, \uNNNN), which keeps a diagnostic on one line.
//
// Everything else is written byte for byte: printable ASCII and other
// Unicode, and bytes that are no valid UTF-8 (a Latin-1 file name), except a
// lone byte 0x80-0x9f, which an 8-bit terminal takes for a C1 control and
// which is written as \xNN. A backslash is not escaped,
// so ordinary paths stay byte-identical and escaping an already escaped text
// changes nothing; the price is that a literal `\n` in a path reads like an
// escaped newline, which can not forge a line.
//
// The header fields (source, hostname, level, timestamp) are not escaped:
// they are set by the process itself.
func writeEscapedDetail(dst *strings.Builder, text string) {
	start := 0
	for i := 0; i < len(text); {
		r, size := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		invalid := r == utf8.RuneError && size == 1
		// A lone byte 0x80-0x9f is no valid UTF-8, but a terminal in an
		// 8-bit locale takes it for a C1 control (0x9b CSI, 0x85 NEL), so it
		// is escaped as \xNN; other invalid bytes (Latin-1 text) stay.
		loneC1 := invalid && text[i] <= 0x9f
		if !loneC1 && (invalid || !needsDiagnosticEscape(r)) {
			i += size
			continue
		}
		dst.WriteString(text[start:i])
		if loneC1 {
			writeDiagnosticByte(dst, text[i])
		} else {
			writeDiagnosticEscape(dst, r)
		}
		i += size
		start = i
	}
	dst.WriteString(text[start:])
}

// needsDiagnosticEscape reports whether r is a control or format character
// that writeEscapedDetail escapes.
func needsDiagnosticEscape(r rune) bool {
	switch {
	case r < 0x20 || r == 0x7f:
		return true
	case r < 0x80:
		return false
	case r <= 0x9f, r == '\u2028', r == '\u2029':
		return true
	default:
		return unicode.Is(unicode.Cf, r)
	}
}

// writeDiagnosticEscape writes the Go escape of r: \n, \r and \t by name,
// other ASCII controls as \xNN and every other rune as \uNNNN.
func writeDiagnosticEscape(dst *strings.Builder, r rune) {
	switch r {
	case '\n':
		dst.WriteString(`\n`)
	case '\r':
		dst.WriteString(`\r`)
	case '\t':
		dst.WriteString(`\t`)
	default:
		var buf [12]byte
		digits := strconv.AppendUint(buf[:0], uint64(r), 16)
		width := 4
		prefix := `\u`
		if r < 0x80 {
			width, prefix = 2, `\x`
		}
		dst.WriteString(prefix)
		for pad := width - len(digits); pad > 0; pad-- {
			dst.WriteByte('0')
		}
		dst.Write(digits)
	}
}

// writeDiagnosticByte writes the escape \xNN of a single byte.
func writeDiagnosticByte(dst *strings.Builder, b byte) {
	const hex = "0123456789abcdef"
	dst.WriteString(`\x`)
	dst.WriteByte(hex[b>>4])
	dst.WriteByte(hex[b&0xf])
}
