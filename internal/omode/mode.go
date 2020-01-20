package omode

import (
	"fmt"
	"os"
	"path"
)

// Mode used.
type Mode int

// Possible modes.
const (
	Unknown      Mode = iota
	Server       Mode = iota
	TailClient   Mode = iota
	CatClient    Mode = iota
	GrepClient   Mode = iota
	MapClient    Mode = iota
	HealthClient Mode = iota
)

// New returns the mode based on the mode string.
func New(modeStr string) Mode {
	switch modeStr {
	case "dserver":
		return Server
	case "server":
		return Server

	case "dtail":
		fallthrough
	case "tail":
		return TailClient

	case "grep":
		fallthrough
	case "dgrep":
		return GrepClient

	case "cat":
		fallthrough
	case "dcat":
		return CatClient

	case "map":
		fallthrough
	case "dmap":
		return MapClient

	case "health":
		return HealthClient

	default:
		panic(fmt.Sprintf("Unknown mode: '%s'", modeStr))
	}
}

// Default mode.
func Default() Mode {
	return New(path.Base(os.Args[0]))
}

func (m Mode) String() string {
	switch m {
	case Server:
		return "server"
	case TailClient:
		return "tail"
	case CatClient:
		return "cat"
	case GrepClient:
		return "grep"
	case MapClient:
		return "map"
	case HealthClient:
		return "health"
	default:
		return "unknown"
	}
}
