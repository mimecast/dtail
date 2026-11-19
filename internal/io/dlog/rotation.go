package dlog

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/mimecast/dtail/internal/source"
)

func startRotation(ctx context.Context, wg *sync.WaitGroup, sourceProcess source.Source,
	rotate func()) {
	if sourceProcess != source.Server {
		return
	}

	rotateCh := make(chan os.Signal, 1)
	signal.Notify(rotateCh, syscall.SIGHUP)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer signal.Stop(rotateCh)
		rotateLoop(ctx, rotateCh, rotate)
	}()
}

// rotateLoop services the log-rotation channel until ctx is cancelled. It is
// split out from startRotation so tests can drive it directly with a fake rotate
// function and a test-owned channel.
func rotateLoop(ctx context.Context, rotateCh <-chan os.Signal, rotate func()) {
	for {
		select {
		case <-rotateCh:
			Common.Debug("Invoking log rotation")
			rotate()
		case <-ctx.Done():
			return
		}
	}
}
