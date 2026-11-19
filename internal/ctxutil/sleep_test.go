package ctxutil

import (
	"context"
	"testing"
	"time"
)

func TestSleepReturnsEarlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if Sleep(ctx, time.Second) {
		t.Fatal("Sleep should stop when the context is canceled")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Sleep took too long to return after cancellation: %v", elapsed)
	}
}

func TestSleepWaitsForDelay(t *testing.T) {
	ctx := context.Background()

	start := time.Now()
	if !Sleep(ctx, 20*time.Millisecond) {
		t.Fatal("Sleep should report success when the delay elapses")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("Sleep returned too early: %v", elapsed)
	}
}
