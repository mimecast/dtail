package integrationtests

import (
	"os"
	"testing"
)

func TestDCat(t *testing.T) {
	testdataFile := "dcat.txt.expected"
	stdoutFile := "dcat.out"
	args := []string{"-spartan", testdataFile}

	if _, err := runCommand(t, "../dcat", args, stdoutFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, stdoutFile, testdataFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(stdoutFile)
}
