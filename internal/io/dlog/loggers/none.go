package loggers

import (
	"context"
	"sync"
	"time"
)

// don't log anything
type none struct{}

func (none) Start(ctx context.Context, wg *sync.WaitGroup) { wg.Done() }

func (none) Log(now time.Time, message string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) LogWithColors(now time.Time, message, coloredMessage string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Raw(now time.Time, message string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) RawWithColors(now time.Time, message, coloredMessage string) {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Flush() {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Pause() {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Resume() {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) Rotate() {
	// This is empty because the none isn't logging but has to satisfy the interface.
}

func (none) SupportsColors() bool { return false }
