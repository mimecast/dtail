package dlog

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/mimecast/dtail/internal/io/dlog/loggers"
)

func rotation(ctx context.Context) {
	rotateCh := make(chan os.Signal, 1)
	signal.Notify(rotateCh, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-rotateCh:
				Common.Debug("Invoking log rotation")
				loggers.FactoryRotate()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}
