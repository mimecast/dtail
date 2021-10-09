package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDTailHealthCheck(t *testing.T) {
	stdoutFile := "dtailhealthcheck.stdout.tmp"
	expectedStdoutFile := "dtailhealthcheck.expected"

	t.Log("Serverless check, is supposed to exit with warning state.")
	exitCode, err := runCommand(t, "../dtailhealthcheck", []string{}, stdoutFile)
	if exitCode != 1 {
		t.Error(fmt.Sprintf("Expected exit code '1' but got '%d': %v", exitCode, err))
		return
	}

	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}

func TestDTailHealthCheck2(t *testing.T) {
	stdoutFile := "dtailhealthcheck2.stdout.tmp"
	expectedStdoutFile := "dtailhealthcheck2.expected"
	args := []string{"--server", "example:1"}

	t.Log("Negative test, is supposed to exit with a critical state.")
	exitCode, err := runCommand(t, "../dtailhealthcheck", args, stdoutFile)
	if exitCode != 2 {
		t.Error(fmt.Sprintf("Expected exit code '2' but got '%d': %v", exitCode, err))
		return
	}

	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}

func TestDTailHealthCheck3(t *testing.T) {
	stdoutFile := "dtailhealthcheck3.stdout.tmp"
	serverStdoutFile := "dtailhealthcheck3.dserver.stdout.tmp"
	expectedStdoutFile := "dtailhealthcheck3.expected"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		args := []string{"--logger", "stdout", "--logLevel", "trace", "--port", "4242"}
		runCommandContext(ctx, t, "../dserver", args, serverStdoutFile)
	}()

	var err error
	args := []string{"--server", "localhost:4242"}
	for i := 0; i < 30; i++ {
		t.Log("Waiting for dserver to start", i)
		time.Sleep(time.Second)
		var exitCode int
		if exitCode, err = runCommand(t, "../dtailhealthcheck", args, stdoutFile); exitCode == 0 {
			break
		}
	}
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(serverStdoutFile)
	os.Remove(stdoutFile)
}
