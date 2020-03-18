package logger

import "github.com/mimecast/dtail/internal/config"

// Strategy allows to specify a log rotation strategy.
type Strategy int

// Possible log strategies.
const (
	NormalStrategy Strategy = iota
	DailyStrategy  Strategy = iota
	StdoutStrategy Strategy = iota
)

func logStrategy() Strategy {
	switch config.Common.LogStrategy {
	case "daily":
		return DailyStrategy
	default:
	}
	return StdoutStrategy
}
