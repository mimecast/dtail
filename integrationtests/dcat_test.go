package integrationtests

import (
	"context"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDCat(t *testing.T) {
	if !config.Env("DTAIL_RUN_INTEGRATION_TESTS") {
		t.Log("Skipping")
		return
	}
	testdataFile := "dcat.txt"
	stdoutFile := "dcat.out"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dcat", "--spartan", "--cfg", "none", testdataFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, testdataFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}

func TestDCat2(t *testing.T) {
	if !config.Env("DTAIL_RUN_INTEGRATION_TESTS") {
		return
	}
	testdataFile := "dcat2.txt"
	expectedFile := "dcat2.txt.expected"
	stdoutFile := "dcat2.out"

	args := []string{"--spartan", "--logLevel", "error", "--cfg", "none"}

	// Cat file 100 times in one session.
	for i := 0; i < 100; i++ {
		args = append(args, testdataFile)
	}

	if _, err := runCommand(context.TODO(), t, stdoutFile, "../dcat", args...); err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContents(t, stdoutFile, expectedFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}

func TestDCatColors(t *testing.T) {
	if !config.Env("DTAIL_RUN_INTEGRATION_TESTS") {
		return
	}

	testdataFile := "dcatcolors.txt"
	stdoutFile := "dcatcolors.out"
	expectedFile := "dcatcolors.expected"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dcat", "--logLevel", "error", "--cfg", "none", testdataFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, expectedFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}
