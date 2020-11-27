package signal

import (
	"context"
	"os"
	gosignal "os/signal"
	"syscall"
	"time"

	"github.com/mimecast/dtail/internal/io/logger"
)

// StatsCh returns a channel for "please print stats" signalling.
func InterruptCh(ctx context.Context) <-chan struct{} {
	sigIntCh := make(chan os.Signal)
	gosignal.Notify(sigIntCh, os.Interrupt)

	sigOtherCh := make(chan os.Signal)
	gosignal.Notify(sigOtherCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)

	statsCh := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigIntCh:
				select {
				case statsCh <- struct{}{}:
					logger.Info("Hint: Hit Ctrl+C twice to exit")
					select {
					case <-sigIntCh:
						os.Exit(0)
					case <-time.After(time.Second):
					}
				default:
					// Stats already printed.
				}
			case <-sigOtherCh:
				os.Exit(0)
			case <-ctx.Done():
				return
			}
		}
	}()

	return statsCh
}
