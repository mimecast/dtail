package fs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRootedPathReadWrite(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "known_hosts")
	rootedPath, pathErr := NewRootedPath(filePath)
	if pathErr != nil {
		t.Fatalf("NewRootedPath failed: %v", pathErr)
	}

	want := []byte("trusted host\n")
	if err := rootedPath.WriteFile(want, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := rootedPath.ReadFile()
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile returned %q, want %q", got, want)
	}
}

func TestRootedPathReadFileRejectsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "ssh")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	outsidePath := filepath.Join(dir, "outside_authorized_keys")
	if err := os.WriteFile(outsidePath, []byte("outside\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	linkPath := filepath.Join(rootDir, "authorized_keys")
	if err := os.Symlink(filepath.Join("..", "outside_authorized_keys"), linkPath); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	rootedPath, err := NewRootedPath(linkPath)
	if err != nil {
		t.Fatalf("NewRootedPath failed: %v", err)
	}

	if _, err := rootedPath.ReadFile(); err == nil {
		t.Fatalf("ReadFile succeeded for escaping symlink")
	}
}
