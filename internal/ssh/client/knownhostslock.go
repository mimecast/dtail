package client

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"syscall"
	"time"
)

const (
	// knownHostsLockTimeout bounds how long an update waits for another
	// client's update of the same known_hosts file. An update holds the lock
	// only for one read-merge-write, so the timeout is only reached when a
	// holder is stuck; the update then goes ahead without the lock.
	knownHostsLockTimeout = 5 * time.Second
	// knownHostsLockMaxRetryDelay caps the backoff between lock attempts.
	knownHostsLockMaxRetryDelay = 20 * time.Millisecond
	// knownHostsTempAttempts is how many unique temporary names are tried
	// before giving up, mirroring os.CreateTemp.
	knownHostsTempAttempts = 10000
)

var errKnownHostsLockTimeout = errors.New("timed out waiting for known hosts lock")

// lockKnownHosts takes an exclusive advisory lock on the file "<name>.lock"
// next to known_hosts inside root, so that concurrent clients serialise their
// read-merge-write cycles instead of overwriting each other's new entries.
// The lock file is persistent: removing it after use would let a waiter lock
// an unlinked inode while a third client creates and locks a fresh one.
//
// The lock file is empty and never written. It is opened read-write where
// possible, because Linux NFS (and CIFS) clients emulate flock with fcntl
// byte-range locks, which need a descriptor open for writing to take an
// exclusive lock. Only when read-write access is refused (a lock file owned by
// another user, or a read-only file system) is it opened read-only, which is
// enough for flock on local file systems. It is created with mode 0644, so
// every user who shares a known_hosts file can lock it; 0600 would lock other
// users out of the lock.
// Platforms without advisory locking (fileLockSupported false) get no lock
// file at all.
//
// The returned release function is always safe to call. A non-nil error means
// the lock is not held (no lock support, lock file not openable, or timeout);
// callers may still update the file, only without the lost-update guard.
func lockKnownHosts(root *os.Root, name string, timeout time.Duration) (func(), error) {
	noop := func() {}
	if !fileLockSupported {
		return noop, fmt.Errorf("lock known hosts file: %w", errors.ErrUnsupported)
	}
	lockFd, err := openLockFile(root, name+".lock")
	if err != nil {
		return noop, fmt.Errorf("open known hosts lock file: %w", err)
	}
	release := func() { _ = lockFd.Close() }

	deadline := time.Now().Add(timeout)
	delay := time.Millisecond
	for {
		locked, lockErr := tryLockFile(lockFd)
		switch {
		case lockErr != nil:
			release()
			return noop, fmt.Errorf("lock known hosts lock file: %w", lockErr)
		case locked:
			return release, nil
		case time.Now().After(deadline):
			release()
			return noop, errKnownHostsLockTimeout
		}
		time.Sleep(delay)
		delay = min(2*delay, knownHostsLockMaxRetryDelay)
	}
}

// createKnownHostsTemp creates a new, uniquely named temporary file next to
// known_hosts inside root. O_EXCL guarantees that no other client writes to
// the same temporary file, which a fixed name could not.
func createKnownHostsTemp(root *os.Root, name string) (*os.File, string, error) {
	for range knownHostsTempAttempts {
		tmpName := fmt.Sprintf("%s.%016x.tmp", name, rand.Uint64())
		fd, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return fd, tmpName, nil
	}
	return nil, "", fmt.Errorf("no unused temporary name for %s: %w", name, os.ErrExist)
}

// openLockFile opens the lock file read-write, creating it if needed, and falls
// back to read-only when read-write access is refused.
func openLockFile(root *os.Root, name string) (*os.File, error) {
	lockFd, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if refused := errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS); !refused {
		return lockFd, err
	}
	return root.OpenFile(name, os.O_RDONLY, 0)
}
