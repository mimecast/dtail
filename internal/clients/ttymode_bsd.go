//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package clients

import "golang.org/x/sys/unix"

// The termios ioctls of the BSDs and macOS (as in golang.org/x/term).
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
