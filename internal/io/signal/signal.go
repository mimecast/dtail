package signal

import (
	"context"
	"os"
	gosignal "os/signal"
	"syscall"
	"time"
)

// InterruptChWithCancel returns a channel for "please print stats" signalling.
// It accepts a cancel function to properly shutdown when termination signals are received.
func InterruptChWithCancel(ctx context.Context, cancel context.CancelFunc,
	interruptPause time.Duration) <-chan string {
	statsCh, _ := startInterruptChWithCancel(ctx, cancel, interruptPause)
	return statsCh
}

// InterruptCh returns a channel for "please print stats" signalling.
//
// Deprecated: Use InterruptChWithCancel for proper cleanup on termination signals.
func InterruptCh(ctx context.Context, interruptPause time.Duration) <-chan string {
	statsCh, _ := startInterruptCh(ctx, interruptPause)
	return statsCh
}

func startInterruptChWithCancel(ctx context.Context,
	cancel context.CancelFunc, interruptPause time.Duration) (<-chan string, <-chan struct{}) {
	return startInterruptHandler(ctx, interruptPause, func() {
		cancel()
		// Wait longer to allow MapReduce cleanup, then force exit if still running.
		go forceExitAfter(time.After(5*time.Second), os.Exit)
	})
}

func startInterruptCh(ctx context.Context, interruptPause time.Duration) (<-chan string, <-chan struct{}) {
	return startInterruptHandler(ctx, interruptPause, func() {
		forceExit(os.Exit)
	})
}

func startInterruptHandler(ctx context.Context, interruptPause time.Duration,
	terminate func()) (<-chan string, <-chan struct{}) {
	sigIntCh := make(chan os.Signal, 10)
	gosignal.Notify(sigIntCh, os.Interrupt)
	sigOtherCh := make(chan os.Signal, 10)
	gosignal.Notify(sigOtherCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)
	statsCh := make(chan string)
	stoppedCh := make(chan struct{})

	go func() {
		defer close(stoppedCh)
		defer gosignal.Stop(sigIntCh)
		defer gosignal.Stop(sigOtherCh)

		runInterruptHandler(ctx, sigIntCh, sigOtherCh, statsCh, interruptPause, terminate)
	}()
	return statsCh, stoppedCh
}

func runInterruptHandler(ctx context.Context, sigIntCh, sigOtherCh <-chan os.Signal,
	statsCh chan<- string, interruptPause time.Duration, terminate func()) {
	for {
		select {
		case <-sigIntCh:
			select {
			case statsCh <- "Hint: Hit Ctrl+C again to exit":
				if !waitForSecondInterrupt(ctx, sigIntCh, interruptPause, terminate) {
					return
				}
			default:
				// Stats already printed.
			}
		case <-sigOtherCh:
			terminate()
		case <-ctx.Done():
			return
		}
	}
}

func waitForSecondInterrupt(ctx context.Context, sigIntCh <-chan os.Signal,
	interruptPause time.Duration, terminate func()) bool {
	timer := time.NewTimer(interruptPause)
	defer timer.Stop()

	select {
	case <-sigIntCh:
		terminate()
	case <-timer.C:
	case <-ctx.Done():
		return false
	}
	return true
}

func forceExitAfter(wait <-chan time.Time, exit func(int)) {
	<-wait
	forceExit(exit)
}

func forceExit(exit func(int)) {
	exit(1)
}

// NoCh doesn't listen on a signal.
func NoCh(ctx context.Context) <-chan string {
	return make(chan string)
}
