package fs

import (
	"fmt"
)

// LineRead represents a read log line.
type LineRead struct {
	// The content of the log line.
	Content []byte
	// Until now, how many log lines were processed?
	Count uint64
	// Sometimes we produce too many log lines so that the client
	// is too slow to process all of them. The server will drop log
	// lines if that happens but it will signal to the client how
	// many log lines in % could be transmitted to the client.
	TransmittedPerc int
	GlobID          *string
}

// Return a human readable representation of the followed line.
func (l LineRead) String() string {
	return fmt.Sprintf("LineRead(Content:%s,TransmittedPerc:%v,Count:%v,GlobID:%s)",
		string(l.Content),
		l.TransmittedPerc,
		l.Count,
		*l.GlobID)
}
