package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func TestRunnerStartHonorsCancellationDuringInitialDelay(t *testing.T) {
	runner := New(config.RuntimeConfig{Server: &config.ServerConfig{}}, jobTestLoggers)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Start(ctx)
	}()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Runner.Start did not stop promptly after cancellation")
	}
}
