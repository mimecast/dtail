package line

import (
	"bytes"
	"fmt"
)

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

// Return a human readable representation of the followed line.
func (l Line) String() string {
	return fmt.Sprintf("Line(Content:%s,TransmittedPerc:%v,Count:%v,SourceID:%s)",
		l.Content.String(),
		l.TransmittedPerc,
		l.Count,
		l.SourceID)
}
