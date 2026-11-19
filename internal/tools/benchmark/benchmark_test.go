package benchmark

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesByModTimeReturnsStatError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	brokenLink := filepath.Join(dir, "baseline_broken.txt")
	if err := os.Symlink(missing, brokenLink); err != nil {
		t.Fatalf("create broken baseline symlink: %v", err)
	}

	_, err := filesByModTime([]string{brokenLink}, true)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("filesByModTime error = %v, want not-exist error", err)
	}
}

func TestShowSimpleDiffReturnsExecutionError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := showSimpleDiff("baseline", "current")
	if err == nil {
		t.Fatal("showSimpleDiff succeeded without diff on PATH")
	}
}
