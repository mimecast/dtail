package line

import (
	"bytes"
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
// Readers use it only where a line is emitted without local context
// (before/after/max), because the context path has to keep lines beyond the
// call. Processors that must retain lines keep implementing only Processor.
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
