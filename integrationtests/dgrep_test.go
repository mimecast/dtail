package integrationtests

import (
	"context"
	"os"
	"testing"
)

func TestDGrep(t *testing.T) {
	inFile := "mapr_testdata.log"
	stdoutFile := "dgrep.stdout.tmp"
	expectedStdoutFile := "dgrep.txt.expected"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dgrep", "--spartan", "--grep", "20211002-071947", inFile)

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

func TestDGrep2(t *testing.T) {
	inFile := "mapr_testdata.log"
	stdoutFile := "dgrep2.stdout.tmp"
	expectedStdoutFile := "dgrep2.txt.expected"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dgrep", "-spartan", "--grep", "20211002-071947", "--invert", inFile)

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

func TestDGrepContext(t *testing.T) {
	inFile := "mapr_testdata.log"
	stdoutFile := "dgrepcontext.stdout.tmp"
	expectedStdoutFile := "dgrepcontext.txt.expected"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dgrep", "--spartan", "--grep", "20211002-071947",
		"-after", "3", "-before", "3", inFile)

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

func TestDGrepContext2(t *testing.T) {
	inFile := "mapr_testdata.log"
	stdoutFile := "dgrepcontext2.stdout.tmp"
	expectedStdoutFile := "dgrepcontext2.txt.expected"

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dgrep", "--spartan", "--grep", "20211002", "-max", "3", inFile)

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
