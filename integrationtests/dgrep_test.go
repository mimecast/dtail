package integrationtests

import (
	"os"
	"testing"
)

func TestDGrep(t *testing.T) {
	testdataFile := "mapr_testdata.log"
	stdoutFile := "dgrep.out"
	expectedResultFile := "dgrep_expected.txt"

	if err := runCommand(t, "../dgrep", []string{"-spartan", "--grep", "20211002-071947", testdataFile}, stdoutFile); err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, expectedResultFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(stdoutFile)
}

func TestDGrep2(t *testing.T) {
	testdataFile := "mapr_testdata.log"
	stdoutFile := "dgrep.out"
	expectedResultFile := "dgrep_expected2.txt"

	if err := runCommand(t, "../dgrep", []string{"-spartan", "--grep", "20211002-071947", "--invert", testdataFile}, stdoutFile); err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, stdoutFile, expectedResultFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(stdoutFile)
}
