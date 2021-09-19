package loggers

import (
	"context"
	"sync"
	"time"
)

type Logger interface {
	Log(now time.Time, message string)
	LogWithColors(now time.Time, message, messageWithColors string)
	Start(ctx context.Context, wg *sync.WaitGroup)
	Flush()
	Pause()
	Resume()
	Rotate()
	SupportsColors() bool
}
