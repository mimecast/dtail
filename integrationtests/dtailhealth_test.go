package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDTailHealthCheck(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	stdoutFile := "dtailhealth.stdout.tmp"
	expectedStdoutFile := "dtailhealth.expected"

	t.Log("Serverless check, is supposed to exit with warning state.")
	exitCode, err := runCommand(context.TODO(), t, stdoutFile, "../dtailhealth")
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
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	stdoutFile := "dtailhealth2.stdout.tmp"
	expectedStdoutFile := "dtailhealth2.expected"

	t.Log("Negative test, is supposed to exit with a critical state.")
	exitCode, err := runCommand(context.TODO(), t, stdoutFile,
		"../dtailhealth", "--server", "example:1")

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
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	stdoutFile := "dtailhealth3.stdout.tmp"
	port := getUniquePortNumber()
	bindAddress := "localhost"
	expectedOut := fmt.Sprintf("OK: All fine at %s:%d :-)", bindAddress, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, _, err := startCommand(ctx, t,
		"../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "trace",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	_, err = runCommandRetry(ctx, t, 10, stdoutFile,
		"../dtailhealth", "--server", fmt.Sprintf("%s:%d", bindAddress, port))
	if err != nil {
		t.Error(err)
		return
	}

	if err := fileContainsStr(t, stdoutFile, expectedOut); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}
