package signal

import (
	"context"
	"os"
	gosignal "os/signal"
	"syscall"
	"time"
)

// StatsCh returns a channel for "please print stats" signalling.
func InterruptCh(ctx context.Context) <-chan string {
	sigIntCh := make(chan os.Signal)
	gosignal.Notify(sigIntCh, os.Interrupt)

	sigOtherCh := make(chan os.Signal)
	gosignal.Notify(sigOtherCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)

	statsCh := make(chan string)

	go func() {
		for {
			select {
			case <-sigIntCh:
				select {
				case statsCh <- "Hint: Hit Ctrl+C again to exit":
					select {
					case <-sigIntCh:
						os.Exit(0)
					case <-time.After(time.Second * 3):
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
