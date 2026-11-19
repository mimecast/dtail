//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package client

import (
	"errors"
	"os"
)

// tryLockFile has no advisory lock on this platform. The caller then updates
// known_hosts without cross-process exclusion: the unique temporary file and
// the atomic rename still avoid errors and torn files, but two clients adding
// hosts at the same moment can drop each other's new entries.
func tryLockFile(*os.File) (bool, error) {
	return false, errors.ErrUnsupported
}

// fileLockSupported reports whether tryLockFile can lock. There is no advisory file lock on this platform.
// It is a variable so that tests can exercise the unsupported path.
var fileLockSupported = false
