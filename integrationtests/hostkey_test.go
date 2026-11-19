package integrationtests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	dtailssh "github.com/mimecast/dtail/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

const (
	// suiteHostKeyFile is the dserver host key every server of the suite
	// uses. Servers started with --cfg none find it at the integration-mode
	// default HostKeyPath (./ssh_host_key, relative to this directory), and
	// tests writing a configuration of their own point HostKeyPath (or the
	// legacy HostKeyFile) at suiteHostKeyPath.
	suiteHostKeyFile = "ssh_host_key"
	// suiteHostKeyBits is the size dserver itself generates by default, so
	// handshakes cost what they cost with a generated key.
	suiteHostKeyBits = 4096
)

// prepareSuiteHostKey makes sure the suite's host key exists before any
// dserver starts, and keeps a valid one from an earlier run, as dserver
// would. Without it every server whose configuration names a fresh path
// generates a 4096-bit RSA key at startup: that takes 0.5-5 s on an idle
// machine and well over the readiness wait of the tests on a loaded one
// ("timed out waiting for dserver ... DTail server not operating properly").
// The key is written to a temporary file and renamed, so a dserver never
// reads a partially written key.
func prepareSuiteHostKey() error {
	if existing, err := os.ReadFile(suiteHostKeyFile); err == nil {
		if _, parseErr := gossh.ParsePrivateKey(existing); parseErr == nil {
			return nil
		}
		_, _ = fmt.Fprintf(os.Stderr, "Replacing unparsable integration host key %s\n", suiteHostKeyFile)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read integration host key %s: %w", suiteHostKeyFile, err)
	}

	privateKey, err := dtailssh.GeneratePrivateRSAKey(suiteHostKeyBits)
	if err != nil {
		return fmt.Errorf("generate integration host key: %w", err)
	}
	return writeFileAtomically(suiteHostKeyFile, dtailssh.EncodePrivateKeyToPEM(privateKey))
}

// writeFileAtomically writes data to a private temporary file next to path
// and renames it to path.
func writeFileAtomically(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.partial")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(tmpPath, path)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", path, writeErr)
	}
	return nil
}

// suiteHostKeyPath returns the absolute path of the suite's host key, for
// server configurations of tests.
func suiteHostKeyPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(suiteHostKeyFile)
	if err != nil {
		t.Fatalf("resolve integration host key path: %v", err)
	}
	return path
}
