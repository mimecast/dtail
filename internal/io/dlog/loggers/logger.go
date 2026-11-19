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
	Flush()
	SupportsColors() bool
}

// Starter is implemented by loggers that own background work.
type Starter interface {
	Start(ctx context.Context, wg *sync.WaitGroup)
}

// Pauser is implemented by terminal loggers whose output must pause while an
// interactive prompt temporarily owns the terminal.
type Pauser interface {
	Pause()
	Resume()
}

// Rotator is implemented by loggers backed by rotatable resources.
type Rotator interface {
	Rotate()
}
