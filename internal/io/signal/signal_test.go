package signal

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	signalHandlerHelperEnv      = "DTAIL_SIGNAL_HANDLER_HELPER"
	signalHandlerReadyMarker    = "signal handler stopped"
	signalHandlerHelperTimeout  = 10 * time.Second
	signalHandlerWaitingForExit = time.Minute
)

func TestInterruptHandlersRestoreDefaultSignalBehavior(t *testing.T) {
	handlers := []string{"with-cancel", "deprecated"}
	signals := []syscall.Signal{
		syscall.SIGINT,
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	}

	for _, handler := range handlers {
		for _, sig := range signals {
			t.Run(handler+"/"+sig.String(), func(t *testing.T) {
				assertDefaultSignalBehavior(t, handler, sig)
			})
		}
	}
}

func TestInterruptHandlersRestoreSIGQUITWithCrashTracebackParent(t *testing.T) {
	t.Setenv("GOTRACEBACK", "crash")
	for _, handler := range []string{"with-cancel", "deprecated"} {
		t.Run(handler, func(t *testing.T) {
			assertDefaultSignalBehavior(t, handler, syscall.SIGQUIT)
		})
	}
}

func TestInterruptHandlersStopOnContextCancellation(t *testing.T) {
	tests := []struct {
		name  string
		start func(context.Context, context.CancelFunc) <-chan struct{}
	}{
		{
			name: "with-cancel",
			start: func(ctx context.Context, cancel context.CancelFunc) <-chan struct{} {
				_, stoppedCh := startInterruptChWithCancel(ctx, cancel, time.Second)
				return stoppedCh
			},
		},
		{
			name: "deprecated",
			start: func(ctx context.Context, _ context.CancelFunc) <-chan struct{} {
				_, stoppedCh := startInterruptCh(ctx, time.Second)
				return stoppedCh
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			stoppedCh := tt.start(ctx, cancel)
			cancel()
			select {
			case <-stoppedCh:
			case <-time.After(signalHandlerHelperTimeout):
				t.Fatal("timed out waiting for signal handler cleanup")
			}
		})
	}
}

func TestInterruptHandlerStopsWhileWaitingForSecondInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigIntCh := make(chan os.Signal)
	sigOtherCh := make(chan os.Signal)
	statsCh := make(chan string, 1)
	stoppedCh := make(chan struct{})
	go func() {
		runInterruptHandler(ctx, sigIntCh, sigOtherCh, statsCh, time.Second, func() {
			t.Error("terminate called after context cancellation")
		})
		close(stoppedCh)
	}()

	sigIntCh <- os.Interrupt
	select {
	case message := <-statsCh:
		if message != "Hint: Hit Ctrl+C again to exit" {
			t.Fatalf("interrupt message = %q", message)
		}
	case <-time.After(signalHandlerHelperTimeout):
		t.Fatal("timed out waiting for first interrupt")
	}

	cancel()
	select {
	case <-stoppedCh:
	case <-time.After(signalHandlerHelperTimeout):
		t.Fatal("signal handler ignored context cancellation while waiting for a second interrupt")
	}
}

func TestInterruptHandlerInvokesTermination(t *testing.T) {
	tests := []struct {
		name    string
		trigger func(*testing.T, chan<- os.Signal, chan<- os.Signal, <-chan string)
	}{
		{
			name: "termination signal",
			trigger: func(_ *testing.T, _ chan<- os.Signal, sigOtherCh chan<- os.Signal,
				_ <-chan string) {
				sigOtherCh <- syscall.SIGTERM
			},
		},
		{
			name: "second interrupt",
			trigger: func(t *testing.T, sigIntCh chan<- os.Signal, _ chan<- os.Signal,
				statsCh <-chan string) {
				sigIntCh <- os.Interrupt
				if message := <-statsCh; message != "Hint: Hit Ctrl+C again to exit" {
					t.Fatalf("interrupt message = %q", message)
				}
				sigIntCh <- os.Interrupt
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			sigIntCh := make(chan os.Signal)
			sigOtherCh := make(chan os.Signal)
			statsCh := make(chan string, 1)
			terminatedCh := make(chan struct{}, 1)
			stoppedCh := make(chan struct{})
			go func() {
				runInterruptHandler(ctx, sigIntCh, sigOtherCh, statsCh, time.Second, func() {
					terminatedCh <- struct{}{}
					cancel()
				})
				close(stoppedCh)
			}()

			tt.trigger(t, sigIntCh, sigOtherCh, statsCh)
			select {
			case <-terminatedCh:
			case <-time.After(signalHandlerHelperTimeout):
				t.Fatal("termination callback was not invoked")
			}
			select {
			case <-stoppedCh:
			case <-time.After(signalHandlerHelperTimeout):
				t.Fatal("signal handler did not stop after termination")
			}
			select {
			case <-terminatedCh:
				t.Fatal("termination callback was invoked more than once")
			default:
			}
		})
	}
}

func TestSignalHandlerHelperProcess(t *testing.T) {
	handler := os.Getenv(signalHandlerHelperEnv)
	if handler == "" {
		return
	}
	if traceback := os.Getenv("GOTRACEBACK"); traceback != "all" {
		t.Fatalf("GOTRACEBACK = %q, want all", traceback)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stoppedCh <-chan struct{}
	switch handler {
	case "with-cancel":
		_, stoppedCh = startInterruptChWithCancel(ctx, cancel, time.Second)
	case "deprecated":
		_, stoppedCh = startInterruptCh(ctx, time.Second)
	default:
		t.Fatalf("unknown signal handler %q", handler)
	}

	cancel()
	select {
	case <-stoppedCh:
	case <-time.After(signalHandlerHelperTimeout):
		t.Fatal("timed out waiting for signal handler cleanup")
	}
	if _, err := fmt.Fprintln(os.Stdout, signalHandlerReadyMarker); err != nil {
		t.Fatalf("write ready marker: %v", err)
	}

	<-time.After(signalHandlerWaitingForExit)
	t.Fatal("process did not receive a signal after handler cleanup")
}

func assertDefaultSignalBehavior(t *testing.T, handler string, sig syscall.Signal) {
	t.Helper()

	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHandlerHelperProcess$")
	cmd.Env = replaceEnvValue(os.Environ(), signalHandlerHelperEnv, handler)
	cmd.Env = replaceEnvValue(cmd.Env, "GOTRACEBACK", "all")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("create helper stdout pipe: %v", err)
	}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	waited := false
	defer func() {
		if waited {
			return
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	type readResult struct {
		line string
		err  error
	}
	readyCh := make(chan readResult, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		readyCh <- readResult{line: strings.TrimSpace(line), err: readErr}
	}()
	select {
	case result := <-readyCh:
		if result.err != nil {
			waitErr := cmd.Wait()
			waited = true
			t.Fatalf("read helper ready marker: %v (wait: %v, stderr: %s)",
				result.err, waitErr, stderr.String())
		}
		if result.line != signalHandlerReadyMarker {
			t.Fatalf("helper ready marker = %q, want %q", result.line, signalHandlerReadyMarker)
		}
	case <-time.After(signalHandlerHelperTimeout):
		t.Fatal("timed out waiting for helper signal cleanup")
	}

	if err := syscall.Kill(cmd.Process.Pid, sig); err != nil {
		t.Fatalf("send %s to helper: %v", sig, err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	select {
	case err := <-waitCh:
		waited = true
		if err == nil {
			t.Fatalf("helper exited successfully after %s", sig)
		}
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("helper status after %s has type %T", sig, cmd.ProcessState.Sys())
		}
		if sig == syscall.SIGQUIT {
			if status.ExitStatus() != 2 || !strings.Contains(stderr.String(), "SIGQUIT") {
				t.Fatalf("SIGQUIT did not produce the runtime's exit status and goroutine dump: %v\n%s",
					cmd.ProcessState, stderr.String())
			}
		} else if !status.Signaled() || status.Signal() != sig {
			t.Fatalf("helper status after %s = %v, want termination by that signal (stderr: %s)",
				sig, cmd.ProcessState, stderr.String())
		}
	case <-time.After(signalHandlerHelperTimeout):
		_ = cmd.Process.Kill()
		waitErr := <-waitCh
		waited = true
		t.Fatalf("helper ignored %s after signal handler cleanup (kill result: %v)", sig, waitErr)
	}
}

func replaceEnvValue(env []string, name, value string) []string {
	prefix := name + "="
	replaced := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			replaced = append(replaced, entry)
		}
	}
	return append(replaced, prefix+value)
}

func TestReplaceEnvValueRemovesInheritedDuplicates(t *testing.T) {
	env := []string{"FIRST=1", "GOTRACEBACK=crash", "SECOND=2", "GOTRACEBACK=none"}
	got := replaceEnvValue(env, "GOTRACEBACK", "all")
	want := "FIRST=1,SECOND=2,GOTRACEBACK=all"
	if joined := strings.Join(got, ","); joined != want {
		t.Fatalf("replaced environment = %q, want %q", joined, want)
	}
}

func TestForceExitUsesFailureStatus(t *testing.T) {
	status := 0
	forceExit(func(code int) { status = code })
	if status != 1 {
		t.Fatalf("forced exit status = %d, want 1", status)
	}
}

func TestForceExitAfterUsesFailureStatusWithoutDelay(t *testing.T) {
	wait := make(chan time.Time)
	close(wait)
	status := 0
	forceExitAfter(wait, func(code int) { status = code })
	if status != 1 {
		t.Fatalf("delayed forced exit status = %d, want 1", status)
	}
}
