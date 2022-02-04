package loggers

import (
	"os"
	"path/filepath"
	"strings"
)

// Rotation is the actual strategy used for log rotation..
type Rotation int

const (
	// DailyRotation tells DTail to rotate its logs on a daily basis or on SIGHUP.
	DailyRotation Rotation = iota
	// SignalRotation tells DTail to rotate its logs only on SIGHUP.
	SignalRotation Rotation = iota
)

// Strategy is a pair of the rotation and the file base.
type Strategy struct {
	// Rotation is the actual rotation strategy used.
	Rotation Rotation
	// FileBase can be a name (e.g. "dserver", "dmap") when signal rotation is used.
	FileBase string
}

// NewStrategy returns the stratey based on its name.
func NewStrategy(name string) Strategy {
	switch strings.ToLower(name) {
	case "daily":
		return Strategy{DailyRotation, ""}
	default:
		return Strategy{SignalRotation, filepath.Base(os.Args[0])}
	}
}
