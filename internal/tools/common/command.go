package common

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// RunCommandWithTimeout runs a command with a timeout
func RunCommandWithTimeout(timeout time.Duration, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-time.After(timeout):
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}
		return fmt.Errorf("command timed out after %v", timeout)
	case err := <-done:
		return err
	}
}
