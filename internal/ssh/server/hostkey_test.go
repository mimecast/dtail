package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/logging"
)

func TestPrivateHostKeyGeneratesAndReloadsExistingKey(t *testing.T) {
	hostKeyFile := filepath.Join(t.TempDir(), "cache", "ssh_host_key")
	if err := os.MkdirAll(filepath.Dir(hostKeyFile), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	hostKeyPath, pathErr := fs.NewRootedPath(hostKeyFile)
	if pathErr != nil {
		t.Fatalf("NewRootedPath failed: %v", pathErr)
	}

	firstPEM, generateErr := generatePrivateHostKey(1024)
	if generateErr != nil {
		t.Fatalf("generatePrivateHostKey failed: %v", generateErr)
	}
	if err := storePrivateHostKey(hostKeyPath, firstPEM); err != nil {
		t.Fatalf("storePrivateHostKey failed: %v", err)
	}

	secondPEM, err := readPrivateHostKey(hostKeyPath)
	if err != nil {
		t.Fatalf("readPrivateHostKey failed: %v", err)
	}
	if !bytes.Equal(secondPEM, firstPEM) {
		t.Fatalf("readPrivateHostKey returned different key data")
	}
}

func TestPrivateHostKeyReturnsConfiguredPathError(t *testing.T) {
	// The explicit path must remain authoritative regardless of test-harness
	// environment state.
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("write parent file: %v", err)
	}

	_, err := PrivateHostKey(filepath.Join(parent, "ssh_host_key"), 1024, logging.NopLogger{})
	if err == nil || !strings.Contains(err.Error(), "private server RSA host key") {
		t.Fatalf("PrivateHostKey error = %v, want configured path error", err)
	}
}
