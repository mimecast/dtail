package integrationtests

import (
	"context"
	"os"
	"testing"
)

func TestDCat(t *testing.T) {
	testdataFile := "dcat.txt"
	stdoutFile := "dcat.out"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dcat", "--spartan", testdataFile)

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
	testdataFile := "dcat2.txt"
	expectedFile := "dcat2.txt.expected"
	stdoutFile := "dcat2.out"

	args := []string{"--spartan", "--logLevel", "error"}

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
	testdataFile := "dcatcolors.txt"
	stdoutFile := "dcatcolors.out"
	expectedFile := "dcatcolors.expected"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dcat", "--logLevel", "error", testdataFile)

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
