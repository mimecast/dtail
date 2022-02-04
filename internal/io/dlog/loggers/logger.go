package loggers

import (
	"context"
	"sync"
	"time"
)

// Logger is there to plug in your own log implementation.
type Logger interface {
	Log(now time.Time, message string)
	LogWithColors(now time.Time, message, messageWithColors string)
	Raw(now time.Time, message string)
	RawWithColors(now time.Time, message, messageWithColors string)
	Start(ctx context.Context, wg *sync.WaitGroup)
	Flush()
	Pause()
	Resume()
	Rotate()
	SupportsColors() bool
}
