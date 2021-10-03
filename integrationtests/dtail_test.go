package integrationtests

import (
	"os"
	"testing"
)

func TestDTailColorTable(t *testing.T) {
	stdoutFile := "dtailcolortable.stdout.tmp"
	expectedStdoutFile := "dtailcolortable.expected"

	if err := runCommand(t, "../dtail", []string{"-colorTable"}, stdoutFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
}
