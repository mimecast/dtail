package main

import (
	"context"
	"testing"
	"time"
)

// TestApplyClientDeadlines verifies that --timeout and --shutdownAfter are turned
// into client-side context deadlines and that the earliest one wins, which is
// what makes the dtail follow client exit instead of reconnecting after a
// timeout-induced disconnect (task xu0).
func TestApplyClientDeadlines(t *testing.T) {
	tests := []struct {
		name          string
		shutdownAfter int
		timeout       int
		wantDeadline  bool
		// wantWithin bounds the deadline from the parent context when a deadline
		// is expected. It reflects the smaller of the two configured windows.
		wantWithin time.Duration
	}{
		{name: "both unset: no deadline", shutdownAfter: 0, timeout: 0, wantDeadline: false},
		{name: "only shutdownAfter", shutdownAfter: 5, timeout: 0, wantDeadline: true, wantWithin: 5 * time.Second},
		{name: "only timeout", shutdownAfter: 0, timeout: 4, wantDeadline: true, wantWithin: 4 * time.Second},
		{name: "timeout smaller wins", shutdownAfter: 30, timeout: 3, wantDeadline: true, wantWithin: 3 * time.Second},
		{name: "shutdownAfter smaller wins", shutdownAfter: 2, timeout: 60, wantDeadline: true, wantWithin: 2 * time.Second},
		{name: "negative values ignored", shutdownAfter: -1, timeout: -1, wantDeadline: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := time.Now()
			ctx, cancel := applyClientDeadlines(context.Background(), tt.shutdownAfter, tt.timeout)
			defer cancel()

			deadline, ok := ctx.Deadline()
			if ok != tt.wantDeadline {
				t.Fatalf("Deadline() ok = %v, want %v", ok, tt.wantDeadline)
			}
			if !tt.wantDeadline {
				return
			}

			// The effective deadline must equal the smaller configured window
			// (allow a small slack for scheduling).
			gotWindow := deadline.Sub(before)
			if gotWindow > tt.wantWithin+time.Second || gotWindow < tt.wantWithin-time.Second {
				t.Fatalf("effective deadline window = %s, want ~%s", gotWindow, tt.wantWithin)
			}
		})
	}
}

// TestApplyClientDeadlinesCancelFires ensures the returned cancel function
// actually cancels the context (releasing timers) without panicking.
func TestApplyClientDeadlinesCancelFires(t *testing.T) {
	ctx, cancel := applyClientDeadlines(context.Background(), 3600, 10)
	cancel()

	select {
	case <-ctx.Done():
	default:
		t.Fatal("context was not canceled after calling cancel()")
	}
}
