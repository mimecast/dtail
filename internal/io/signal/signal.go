package signal

import (
	"context"
	"os"
	gosignal "os/signal"
	"syscall"
)

// StatsCh returns a channel for "please print stats" signalling.
func StatsCh(ctx context.Context) <-chan struct{} {
	sigCh := make(chan os.Signal)
	gosignal.Notify(sigCh, syscall.SIGINFO, syscall.SIGUSR1)

	statsCh := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigCh:
				select {
				case statsCh <- struct{}{}:
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
