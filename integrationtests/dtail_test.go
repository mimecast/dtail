package integrationtests

import (
	"os"
	"testing"
)

func TestDTailColorTable(t *testing.T) {
	stdoutFile := "dtailcolortable.stdout.tmp"
	expectedStdoutFile := "dtailcolortable.expected"
	args := []string{"-colorTable"}

	if _, err := runCommand(t, "../dtail", args, stdoutFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(stdoutFile)
}
