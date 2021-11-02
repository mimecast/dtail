package loggers

import (
	"context"
	"sync"
	"time"
)

// don't log anything
type none struct{}

func (none) Start(ctx context.Context, wg *sync.WaitGroup)               { wg.Done() }
func (none) Log(now time.Time, message string)                           {}
func (none) LogWithColors(now time.Time, message, coloredMessage string) {}
func (none) Raw(now time.Time, message string)                           {}
func (none) RawWithColors(now time.Time, message, coloredMessage string) {}
func (none) Flush()                                                      {}
func (none) Pause()                                                      {}
func (none) Resume()                                                     {}
func (none) Rotate()                                                     {}
func (none) SupportsColors() bool                                        { return false }
