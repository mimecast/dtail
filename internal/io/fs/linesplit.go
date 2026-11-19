package fs

import (
	"bytes"
	"context"
	"math"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
)

// The line splitting of snapshot reads, shared by ReadFile and the journal
// reader (internal/io/journal), so that a journal: target cuts its entries
// at MaxLineLength exactly like a file read cuts its lines.

// minReadScannerTokenSize is the smallest maximum token size of a scanner
// splitting with SplitLine: the capacity of the pooled buffer it starts with,
// which holds a line of the default MaxLineLength (1 MiB) and its newline.
// The actual maximum follows the configured MaxLineLength (see
// ScannerTokenLimit), so the scanner never asks for a line longer than its
// buffer may grow.
const minReadScannerTokenSize = pool.ScannerBufferCapacity

// LineEnd tells how a token SplitLine returned ends.
type LineEnd int

const (
	// LineEndNone means that SplitLine returned no token: it needs more data,
	// or there is none left.
	LineEndNone LineEnd = iota
	// LineEndNewline means that the token is a whole line of up to the limit,
	// with its newline.
	LineEndNewline
	// LineEndCut means that the token is the first limit bytes of a longer
	// line, without a line terminator; the rest of the line follows.
	LineEndCut
	// LineEndEOF means that the token is the last line of the input, of up to
	// the limit, without a newline after it.
	LineEndEOF
)

// LineLimit returns the line length a reader configured with maxLineLength
// (Server.MaxLineLength) cuts lines at: maxLineLength, or the 1 MiB default
// when it is not positive.
func LineLimit(maxLineLength int) int {
	if maxLineLength <= 0 {
		return defaultMaxLineLength
	}
	return maxLineLength
}

// ScannerTokenLimit returns the maximum token size of a bufio.Scanner that
// splits with SplitLine at limit: one byte more than the limit, so that a
// line of exactly limit bytes fits together with its newline, but at least
// minReadScannerTokenSize, the capacity of the pooled buffer. SplitLine asks
// for more data while it holds at most limit bytes without a newline and cuts
// a line once it holds more, so a scanner buffer of this size never fills up
// without a token; a smaller one would make bufio.Scanner fail with
// ErrTooLong on a line longer than the buffer but shorter than the limit (a
// limit above 1 MiB), ending the read and losing every following line.
//
// The one extra byte matters when the buffer would otherwise fill at exactly
// limit bytes: only the extra byte lets the scanner see whether a newline
// follows, so that a line of exactly the limit stays whole. With the default
// limit, the pooled buffer has that byte already.
//
// The scanner grows its buffer by doubling, but never past this limit, so the
// buffer a reader holds is at most about twice the longest line it read,
// capped at limit + 1 bytes.
func ScannerTokenLimit(limit int) int {
	if limit == math.MaxInt {
		return limit
	}
	return max(limit+1, minReadScannerTokenSize)
}

// SplitLine is the core of a bufio.SplitFunc that respects a maximum line
// length: a line of up to limit bytes before its newline (or the end of the
// input) is one token, a longer one is cut into tokens of exactly limit
// bytes. The returned LineEnd tells which kind of token it returned, so that
// the caller can warn about a cut line (LineEndCut) and treat an unterminated
// last line (LineEndEOF) as it needs to. The "\r" of a CRLF ending counts
// towards the limit like any other byte.
func SplitLine(data []byte, atEOF bool, limit int) (advance int, token []byte, end LineEnd) {
	if atEOF && len(data) == 0 {
		return 0, nil, LineEndNone
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		if i > limit {
			return limit, data[:limit], LineEndCut
		}
		return i + 1, data[:i+1], LineEndNewline
	}
	// Without a newline in sight, a line of up to limit bytes may still end
	// with the next byte, so ask for more data (ScannerTokenLimit leaves room
	// for it) unless this is the final, non-terminated line; only more than
	// limit bytes are a line too long, which matches the "i > limit" check
	// above and the follow read (followLineProcessor.processChunk).
	if len(data) > limit {
		return limit, data[:limit], LineEndCut
	}
	if atEOF {
		return len(data), data, LineEndEOF
	}
	return 0, nil, LineEndNone
}

// SendLongLineWarning sends the long-line warning about filePath (see
// LongLineWarning) to serverMessages, the client of the read. With no
// channel there is nobody to tell, and it neither sends nor logs anything.
// It returns false when ctx is done before the warning could be sent.
func SendLongLineWarning(ctx context.Context, logger logging.Logger, filePath string,
	serverMessages chan<- string) bool {

	if serverMessages == nil {
		return true
	}
	select {
	case serverMessages <- LongLineWarning(logger, filePath):
		return true
	case <-ctx.Done():
		return false
	}
}
