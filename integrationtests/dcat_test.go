package integrationtests

import (
	"os"
	"testing"
)

func TestDCat(t *testing.T) {
	testdataFile := "testdata.txt"
	stdoutFile := "dcat.out"

	if err := runCommand(t, "../dcat", []string{"-spartan", testdataFile}, stdoutFile); err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, testdataFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(stdoutFile)
}
