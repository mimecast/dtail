package integrationtests

import (
	"context"
	"os"
	"testing"
)

func TestDCat(t *testing.T) {
	testdataFile := "dcat.txt.expected"
	stdoutFile := "dcat.out"

	_, err := runCommand(context.TODO(), stdoutFile,
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
