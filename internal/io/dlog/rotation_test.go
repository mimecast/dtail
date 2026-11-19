package dlog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/source"
)

const (
	rotationHelperEnv      = "DTAIL_ROTATION_SIGNAL_HELPER"
	rotationReadyMarker    = "rotation setup complete"
	rotationHelperTimeout  = 10 * time.Second
	rotationWaitingForExit = time.Minute
)

func TestStartRotationRegistersSIGHUPForServer(t *testing.T) {
	prev := Common
	Common = &DLog{maxLevel: None}
	t.Cleanup(func() { Common = prev })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	rotatedCh := make(chan struct{}, 1)
	startRotation(ctx, &wg, source.Server, func() {
		rotatedCh <- struct{}{}
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	select {
	case <-rotatedCh:
	case <-time.After(rotationHelperTimeout):
		t.Fatal("server log rotation did not receive SIGHUP")
	}

	cancel()
	waitForRotationStop(t, &wg)
}

func TestRotationDoesNotRetainSIGHUP(t *testing.T) {
	for _, scenario := range []string{"client-active", "health-check-active", "server-stopped"} {
		t.Run(scenario, func(t *testing.T) {
			assertDefaultSIGHUPBehavior(t, scenario)
		})
	}
}

func TestRotationSignalHelperProcess(t *testing.T) {
	scenario := os.Getenv(rotationHelperEnv)
	if scenario == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var sourceProcess source.Source
	switch scenario {
	case "client-active":
		sourceProcess = source.Client
	case "health-check-active":
		sourceProcess = source.HealthCheck
	case "server-stopped":
		sourceProcess = source.Server
	default:
		t.Fatalf("unknown rotation helper scenario %q", scenario)
	}
	startRotation(ctx, &wg, sourceProcess, func() {})
	if sourceProcess == source.Server {
		cancel()
		waitForRotationStop(t, &wg)
	}

	if _, err := fmt.Fprintln(os.Stdout, rotationReadyMarker); err != nil {
		t.Fatalf("write ready marker: %v", err)
	}
	<-time.After(rotationWaitingForExit)
	t.Fatal("process did not receive SIGHUP")
}

func assertDefaultSIGHUPBehavior(t *testing.T, scenario string) {
	t.Helper()

	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestRotationSignalHelperProcess$")
	cmd.Env = append(os.Environ(), rotationHelperEnv+"="+scenario)
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
		if result.line != rotationReadyMarker {
			t.Fatalf("helper ready marker = %q, want %q", result.line, rotationReadyMarker)
		}
	case <-time.After(rotationHelperTimeout):
		t.Fatal("timed out waiting for rotation setup")
	}

	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP to helper: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	select {
	case err := <-waitCh:
		waited = true
		if err == nil {
			t.Fatal("helper exited successfully after SIGHUP")
		}
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGHUP {
			t.Fatalf("helper status = %v, want termination by SIGHUP (stderr: %s)",
				cmd.ProcessState, stderr.String())
		}
	case <-time.After(rotationHelperTimeout):
		_ = cmd.Process.Kill()
		waitErr := <-waitCh
		waited = true
		t.Fatalf("helper ignored SIGHUP after rotation setup (kill result: %v)", waitErr)
	}
}

func waitForRotationStop(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(rotationHelperTimeout):
		t.Fatal("timed out waiting for log rotation shutdown")
	}
}

// TestRotateLoopHandlesMultipleSignals is a regression test for a bug where
// rotateLoop returned after the first signal, so subsequent SIGHUPs were
// silently dropped. Sending two signals on rotateCh must result in two
// rotate() invocations before ctx is cancelled.
func TestRotateLoopHandlesMultipleSignals(t *testing.T) {
	prev := Common
	Common = &DLog{maxLevel: None}
	t.Cleanup(func() { Common = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rotateCh := make(chan os.Signal, 2)
	var count int32
	rotated := make(chan struct{}, 2)
	rotate := func() {
		atomic.AddInt32(&count, 1)
		rotated <- struct{}{}
	}

	done := make(chan struct{})
	go func() {
		rotateLoop(ctx, rotateCh, rotate)
		close(done)
	}()

	rotateCh <- os.Interrupt
	waitForRotate(t, rotated, "first")
	rotateCh <- os.Interrupt
	waitForRotate(t, rotated, "second")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rotateLoop did not return after ctx cancel")
	}

	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("rotate() called %d times, want 2", got)
	}
}

func waitForRotate(t *testing.T, rotated <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-rotated:
	case <-time.After(2 * time.Second):
		t.Fatalf("rotate() was not invoked for %s signal", label)
	}
}
