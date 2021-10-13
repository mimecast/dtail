package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestDTailHealthCheck(t *testing.T) {
	stdoutFile := "dtailhealthcheck.stdout.tmp"
	expectedStdoutFile := "dtailhealthcheck.expected"

	t.Log("Serverless check, is supposed to exit with warning state.")
	exitCode, err := runCommand(context.TODO(), t, stdoutFile, "../dtailhealthcheck")
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

	t.Log("Negative test, is supposed to exit with a critical state.")
	exitCode, err := runCommand(context.TODO(), t, stdoutFile,
		"../dtailhealthcheck", "--server", "example:1")

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
	expectedStdoutFile := "dtailhealthcheck3.expected"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, _, err := startCommand(ctx, t,
		"../dserver",
		"--logger", "stdout",
		"--logLevel", "trace",
		"--port", "4242",
	)
	if err != nil {
		t.Error(err)
		return
	}

	_, err = runCommandRetry(ctx, t, 10, stdoutFile,
		"../dtailhealthcheck", "--server", "localhost:4242")
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}
