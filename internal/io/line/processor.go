package line

import (
	"bytes"
	"os"
)

// Processor defines an interface for processing lines read from files.
// This interface replaces the channel-based approach for better performance.
type Processor interface {
	// ProcessLine handles a single line read from a file.
	// The line buffer ownership is transferred to the processor.
	// Returns error if processing should stop.
	ProcessLine(line *bytes.Buffer, lineNum uint64, sourceID string) error

	// Flush ensures any buffered data is written out.
	// Called when file reading completes or on periodic intervals.
	Flush() error

	// Close cleans up any resources used by the processor.
	// Called when processing is complete.
	Close() error
}

// RawProcessor is an optional extension of Processor for processors that can
// consume a line straight from the reader's transient byte slice, skipping the
// pooled-buffer copy that ProcessLine requires.
//
// Unlike ProcessLine, ProcessRawLine does not transfer ownership: raw is
// borrowed and only valid for the duration of the call (it typically aliases a
// bufio.Scanner token or a reused partial-line buffer). An implementation must
// copy whatever it needs before returning and must not retain raw or any
// sub-slice of it. There is nothing to recycle on any return path.
//
// Readers use it when no before-context is requested. Max/after-only context
// does not retain input and can also borrow emitted lines. Processors that
// must retain lines keep implementing only Processor.
type RawProcessor interface {
	ProcessRawLine(raw []byte, lineNum uint64, sourceID string) error
}

// SourceRestarter is an optional extension of Processor for processors that
// keep state for the source they are fed from. A follow-mode reader that
// detects an in-place truncation (copytruncate) rewinds the same descriptor
// and keeps feeding the same Processor, so without this notification such a
// processor could not tell the rewritten content from the old one.
//
// The reader calls SourceRestarted from the goroutine that calls ProcessLine,
// after the last line of the old content and before the first line of the new
// content. Readers do not call it when a source ends or is reopened with a new
// Processor (rotation), because that Processor starts with fresh state anyway.
type SourceRestarter interface {
	SourceRestarted()
}

// PositionObserver is an optional extension of Processor for processors that
// must know where in the source file each line they were fed ends, for example
// so that a consumer can later resume the read with a reader of its own at an
// exact line boundary.
//
// A follow-mode reader of an uncompressed file calls LineEndsAt from the
// goroutine that calls ProcessLine, right after it passed a line to its
// filter, with the byte offset just past that line in the file (past its
// newline, or the split point of a line longer than the maximum line length)
// and the identity of the open file. It does so whether or not the filter
// passed the line on, so an observer that pairs offsets with the lines it got
// must be fed through a filter that passes every line. The reader does not
// call it for a trailing fragment it feeds when its read is cancelled. Readers
// of compressed files and snapshot readers never call it.
//
// The same reader calls ReadUpTo, on the same goroutine, once it opened the
// file, with the offset it starts reading at, and after every read, before it
// feeds that read's lines, with the offset it has read the file up to
// (including bytes it buffered but has not fed yet). An observer can so tell
// which of the lines fed later were read before a given moment. It calls
// ReadStarting right before every read, so that from then until the ReadUpTo
// after it an observer knows the reader may have read further than ReadUpTo
// said last; a read that fails before its position is known is followed by
// the end of the reader's run instead.
type PositionObserver interface {
	LineEndsAt(offset int64, file os.FileInfo)
	ReadStarting()
	ReadUpTo(offset int64, file os.FileInfo)
}
