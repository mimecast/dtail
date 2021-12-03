package loggers

import (
	"context"
	"sync"
	"time"
)

// don't log anything
type none struct{}

func (none) Start(ctx context.Context, wg *sync.WaitGroup) { wg.Done() }

// Log is empty because the none isn't logging but has to statify the interface.
func (none) Log(now time.Time, message string) {}

// LogWithColora is empty because the none isn't logging but has to statify the interface.
func (none) LogWithColors(now time.Time, message, coloredMessage string) {}

// Raw is empty because the none isn't logging but has to statify the interface.
func (none) Raw(now time.Time, message string) {}

// RawWithColors is empty because the none isn't logging but has to statify the interface.
func (none) RawWithColors(now time.Time, message, coloredMessage string) {}

// Flush is empty because the none isn't logging but has to statify the interface.
func (none) Flush() {}

// Pause is empty because the none isn't logging but has to statify the interface.
func (none) Pause() {}

// Resume is empty because the none isn't logging but has to statify the interface.
func (none) Resume() {}

// Rotate is empty because the none isn't logging but has to statify the interface.
func (none) Rotate() {}

func (none) SupportsColors() bool { return false }
