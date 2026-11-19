//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package client

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestOpenLockFileIsReadWriteWhenAllowed checks that a lock file the user may
// write is opened for writing: Linux NFS and CIFS clients emulate flock with
// fcntl byte-range locks, which refuse an exclusive lock on a read-only
// descriptor.
func TestOpenLockFileIsReadWriteWhenAllowed(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}
	defer func() { _ = root.Close() }()

	lockFd, err := openLockFile(root, "known_hosts.lock")
	if err != nil {
		t.Fatalf("openLockFile failed: %v", err)
	}
	defer func() { _ = lockFd.Close() }()

	flags, err := unix.FcntlInt(lockFd.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("F_GETFL failed: %v", err)
	}
	if mode := flags & unix.O_ACCMODE; mode != unix.O_RDWR {
		t.Fatalf("lock file access mode = %#o, want O_RDWR (%#o)", mode, unix.O_RDWR)
	}
}
