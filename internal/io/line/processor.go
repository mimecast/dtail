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
