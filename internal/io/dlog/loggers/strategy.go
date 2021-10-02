package loggers

import (
	"os"
	"path/filepath"
)

type Rotation int

const (
	DailyRotation  Rotation = iota
	SignalRotation Rotation = iota
)

type Strategy struct {
	Rotation Rotation
	FileBase string
}

func GetStrategy(name string) Strategy {
	switch name {
	case "daily":
		return Strategy{DailyRotation, ""}
	default:
		return Strategy{SignalRotation, filepath.Base(os.Args[0])}
	}
}
