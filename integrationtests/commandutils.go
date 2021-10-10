package integrationtests

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// The exit code and the Go error of the command terminated.
type exitPromise func() (int, error)

func runCommand(ctx context.Context, stdoutFile, cmdStr string,
	args ...string) (int, error) {

	stdinCh, _, exit, err := startCommand(ctx, cmdStr, args...)
	if err != nil {
		return -1, err
	}

	fd, err := os.Create(stdoutFile)
	if err != nil {
		return -2, err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()

	go func() {
		defer fd.Close()
		defer wg.Done()
		for line := range stdinCh {
			fd.WriteString(line)
			fd.WriteString("\n")
		}
	}()

	return exit()
}

func runCommandRetry(ctx context.Context, retries int, stdoutFile, cmd string,
	args ...string) (exitCode int, err error) {

	for i := 0; i < retries; i++ {
		time.Sleep(time.Second)
		if exitCode, err = runCommand(ctx, stdoutFile, cmd, args...); exitCode == 0 {
			return
		}
	}
	return
}

func startCommand(ctx context.Context, cmdStr string,
	args ...string) (<-chan string, <-chan string, exitPromise, error) {

	stdoutCh := make(chan string)
	stderrCh := make(chan string)

	if _, err := os.Stat(cmdStr); err != nil {
		return stdoutCh, stderrCh, nil,
			fmt.Errorf("no such executable '%s', please compile first: %v", cmdStr, err)
	}

	cmd := exec.CommandContext(ctx, cmdStr, args...)

	cmdStdout, err := cmd.StdoutPipe()
	if err != nil {
		return stdoutCh, stderrCh, nil, err
	}
	cmdStderr, err := cmd.StderrPipe()
	err = cmd.Start()
	if err != nil {
		return stdoutCh, stderrCh, nil, err
	}

	go func() {
		defer close(stdoutCh)
		scanner := bufio.NewScanner(cmdStdout)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			stdoutCh <- scanner.Text()
		}
	}()
	go func() {
		close(stderrCh)
		scanner := bufio.NewScanner(cmdStderr)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			stderrCh <- scanner.Text()
		}
	}()

	return stdoutCh, stderrCh, func() (int, error) {
		err := cmd.Wait()
		return exitCodeFromError(err), err
	}, nil
}

func exitCodeFromError(err error) int {
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			return ws.ExitStatus()
		}
	}
	return 0
}
