package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDTailHealth1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	outFile := "dtailhealth1.stdout.tmp"
	expectedOutFile := "dtailhealth1.expected"

	t.Log("Serverless check, is supposed to exit with warning state.")
	exitCode, err := runCommand(context.TODO(), t, outFile, "../dtailhealth")
	if exitCode != 1 {
		t.Error(fmt.Sprintf("Expected exit code '1' but got '%d': %v", exitCode, err))
		return
	}

	if err := compareFiles(t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(outFile)
}

func TestDTailHealth2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	outFile := "dtailhealth2.stdout.tmp"
	expectedOutFile := "dtailhealth2.expected"

	t.Log("Negative test, is supposed to exit with a critical state.")
	exitCode, err := runCommand(context.TODO(), t, outFile,
		"../dtailhealth", "--server", "example:1")

	if exitCode != 2 {
		t.Error(fmt.Sprintf("Expected exit code '2' but got '%d': %v", exitCode, err))
		return
	}

	if err := compareFiles(t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(outFile)
}

func TestDTailHealthCheck3(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	outFile := "dtailhealth3.stdout.tmp"
	port := getUniquePortNumber()
	bindAddress := "localhost"
	expectedOut := fmt.Sprintf("OK: All fine at %s:%d :-)", bindAddress, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
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

	_, err = runCommandRetry(ctx, t, 10, outFile,
		"../dtailhealth", "--server", fmt.Sprintf("%s:%d", bindAddress, port))
	if err != nil {
		t.Error(err)
		return
	}

	if err := fileContainsStr(t, outFile, expectedOut); err != nil {
		t.Error(err)
		return
	}

	os.Remove(outFile)
}
