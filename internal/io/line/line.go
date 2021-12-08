package line

import (
	"bytes"
	"fmt"
	"sync"
)

// lineBuffer is there to optimize memory allocations. DTail otherwise allocates
// a lot of memory while reading logs.
var lineBuffer = sync.Pool{
	New: func() interface{} {
		return &Line{}
	},
}

// Line represents a read log line.
type Line struct {
	// The content of the log line.
	Content *bytes.Buffer
	// Until now, how many log lines were processed?
	Count uint64
	// Sometimes we produce too many log lines so that the client
	// is too slow to process all of them. The server will drop log
	// lines if that happens but it will signal to the client how
	// many log lines in % could be transmitted to the client.
	TransmittedPerc int
	// Contains the unique identifier of the source log file.
	// It could be the name of the log or it could be one of the parent
	// directories in case multiple log files with the same basename are
	// followed.
	SourceID string
}

// New creaters a new line object. This is a DTail internal helper structure for reading files.
func New(content *bytes.Buffer, count uint64, transmittedPerc int, sourceID string) *Line {
	l := lineBuffer.Get().(*Line)
	l.Content = content
	l.Count = count
	l.TransmittedPerc = transmittedPerc
	l.SourceID = sourceID
	return l
}

// Null returns a new line with all members initialized to their null value.
func Null() *Line {
	l := lineBuffer.Get().(*Line)
	l.NullValues()
	return l
}

// Return a human readable representation of the followed line.
func (l Line) String() string {
	return fmt.Sprintf("Line(Content:%s,TransmittedPerc:%v,Count:%v,SourceID:%s)",
		l.Content.String(),
		l.TransmittedPerc,
		l.Count,
		l.SourceID)
}

// Recycle the line. Once done, don't reuse this instance!!!
func (l *Line) Recycle() {
	// No explicit reset required, as NewLine overrides all elements
	// already takes care of it.
	//l.Reset()
	lineBuffer.Put(l)
}

// NullValues nulls all line struct members to their default state.
func (l *Line) NullValues() {
	l.Content = nil
	l.Count = 0
	l.TransmittedPerc = 0
	l.SourceID = ""
}
