package signal

import (
	"context"
	"os"
	gosignal "os/signal"
	"time"

	"github.com/mimecast/dtail/internal/io/logger"
)

// StatsCh returns a channel for "please print stats" signalling.
func InterruptCh(ctx context.Context) <-chan struct{} {
	sigCh := make(chan os.Signal)
	gosignal.Notify(sigCh, os.Interrupt)

	statsCh := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigCh:
				select {
				case statsCh <- struct{}{}:
					logger.Info("Hit Ctrl+C twice to exit")
					select {
					case <-sigCh:
						os.Exit(0)
					case <-time.After(time.Second):
					}
				default:
					// Stats currently already printed.
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return statsCh
}
