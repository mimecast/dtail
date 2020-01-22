package omode

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
	ExecClient   Mode = iota
)

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
	case ExecClient:
		return "exec"
	default:
		return "unknown"
	}
}
