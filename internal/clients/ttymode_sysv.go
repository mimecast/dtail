//go:build aix || linux || solaris || zos

package clients

import "golang.org/x/sys/unix"

// The termios ioctls of Linux and the other System V style systems (as in
// golang.org/x/term).
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
