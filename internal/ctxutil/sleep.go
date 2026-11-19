package ctxutil

import (
	"context"
	"time"
)

// Sleep waits for the delay or exits early when the context is canceled.
// It returns true when the full delay elapsed.
func Sleep(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
