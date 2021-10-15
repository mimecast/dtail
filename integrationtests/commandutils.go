package integrationtests

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The exit code and the Go error of the command terminated.
type exitPromise func() (int, error)

func runCommand(ctx context.Context, t *testing.T, stdoutFile, cmdStr string,
	args ...string) (int, error) {

	if _, err := os.Stat(cmdStr); err != nil {
		return 0, fmt.Errorf("no such executable '%s', please compile first: %v", cmdStr, err)
	}

	fd, err := os.Create(stdoutFile)
	if err != nil {
		return 0, nil
	}
	defer fd.Close()

	t.Log(cmdStr, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, cmdStr, args...)
	out, err := cmd.CombinedOutput()

	fd.Write(out)

	return exitCodeFromError(err), err
}

func runCommandRetry(ctx context.Context, t *testing.T, retries int, stdoutFile,
	cmd string, args ...string) (exitCode int, err error) {

	for i := 0; i < retries; i++ {
		time.Sleep(time.Second)
		if exitCode, err = runCommand(ctx, t, stdoutFile, cmd, args...); exitCode == 0 {
			return
		}
	}
	return
}

func startCommand(ctx context.Context, t *testing.T, cmdStr string,
	args ...string) (<-chan string, <-chan string, exitPromise, error) {

	stdoutCh := make(chan string)
	stderrCh := make(chan string)

	if _, err := os.Stat(cmdStr); err != nil {
		return stdoutCh, stderrCh, nil,
			fmt.Errorf("no such executable '%s', please compile first: %v", cmdStr, err)
	}

	t.Log(cmdStr, strings.Join(args, " "))
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
		scanner := bufio.NewScanner(cmdStdout)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			stdoutCh <- scanner.Text()
		}
	}()
	go func() {
		scanner := bufio.NewScanner(cmdStderr)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			stderrCh <- scanner.Text()
		}
		close(stderrCh)
	}()

	return stdoutCh, stderrCh, func() (int, error) {
		err := cmd.Wait()
		return exitCodeFromError(err), err
	}, nil
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		ws := exitError.Sys().(syscall.WaitStatus)
		return ws.ExitStatus()
	}
	panic(fmt.Sprintf("Unable to get process exit code from error: %v", err))
}
