//go:build linux

package journal

import (
	"bytes"
	"fmt"
)

// stderrTruncatedNote ends the server message of a journalctl stderr line
// that was cut at maxStderrTokenSize.
var stderrTruncatedNote = fmt.Sprintf(" [truncated: stderr line longer than %d bytes]", maxStderrTokenSize)

// stderrLineSplitter splits journalctl's stderr into lines for bufio.Scanner
// like bufio.ScanLines (without the "\n" and a "\r" before it), except that a
// line longer than limit bytes does not end the scan with bufio.ErrTooLong:
// its first limit bytes are one token, marked truncated, and the rest of the
// line up to its newline is drained and dropped. So one oversized diagnostic
// line can neither cancel the journalctl run nor block the stderr pipe.
type stderrLineSplitter struct {
	limit int
	// discarding drops the rest of a truncated line until its newline.
	discarding bool
	// truncated reports that the last token is the start of a longer line.
	truncated bool
}

// split is the bufio.SplitFunc of the splitter. The scanner's buffer must be
// capped at limit+1 bytes (scanner.Buffer), so that the scanner calls split
// with more than limit bytes before it gives up on a line without newline: a
// line of exactly limit bytes is only known to fit once its newline is in
// the buffer, which needs room for limit+1 bytes. A line of exactly limit
// bytes ended by "\r\n" needs limit+2 bytes and so is cut (without its "\r")
// and marked truncated.
func (s *stderrLineSplitter) split(data []byte, atEOF bool) (int, []byte, error) {
	s.truncated = false
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		if s.discarding {
			s.discarding = false
			return i + 1, nil, nil
		}
		line := dropCR(data[:i])
		if len(line) > s.limit {
			// A whole line above the limit can reach split at once when the
			// buffer is larger than the limit. A "\r" before the newline does
			// not count, so a "\r\n" line of up to limit bytes is whole.
			s.truncated = true
			return i + 1, line[:s.limit], nil
		}
		return i + 1, line, nil
	}
	if s.discarding {
		return len(data), nil, nil
	}
	// Exactly limit bytes without a newline may still be a whole line whose
	// newline is the next byte, so only more than limit bytes cut the line.
	if len(data) > s.limit {
		s.discarding = true
		s.truncated = true
		return len(data), data[:s.limit], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), dropCR(data), nil
	}
	return 0, nil, nil
}

// dropCR drops a terminal "\r", as bufio.ScanLines does.
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[:len(data)-1]
	}
	return data
}

// stderrMessage formats one stderr line as a server message.
func stderrMessage(line []byte, truncated bool) string {
	if truncated {
		return fmt.Sprintf("journalctl stderr: %s%s\n", line, stderrTruncatedNote)
	}
	return fmt.Sprintf("journalctl stderr: %s\n", line)
}
