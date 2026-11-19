//go:build darwin || dragonfly || freebsd || netbsd || openbsd || aix || linux || solaris || zos

package clients

import (
	"os"
	gosignal "os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// cbreakTTY is a terminal switched to "cbreak" mode for the interactive line
// editor. Canonical line buffering and echo are off, so that the editor sees
// every key (Tab, arrows) and echoes the line itself. Unlike
// golang.org/x/term's MakeRaw it keeps
//   - ISIG: Ctrl-C and Ctrl-Z still send SIGINT and SIGTSTP, which the
//     interrupt statistics and the shutdown rely on;
//   - OPOST/ONLCR: log lines written to the same terminal keep "\n" -> "\r\n",
//     so they do not staircase;
//   - ICRNL: Enter arrives as "\n", which the editor accepts as well.
type cbreakTTY struct {
	fd            int
	saved, cbreak unix.Termios
}

// enterCbreakMode switches the terminal fd to cbreak mode. The caller
// restores the previous mode with restore.
func enterCbreakMode(fd int) (ttyMode, error) {
	old, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	mode := &cbreakTTY{fd: fd, saved: *old, cbreak: *old}
	mode.cbreak.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHONL
	mode.cbreak.Cc[unix.VMIN] = 1
	mode.cbreak.Cc[unix.VTIME] = 0
	if err := mode.reapply(); err != nil {
		return nil, err
	}
	return mode, nil
}

// reapply sets cbreak mode again, e.g. after the job was suspended and
// continued: bash and zsh leave the terminal in their own (cooked) mode on
// "fg". It does nothing while dtail runs in the background (see foreground).
func (m *cbreakTTY) reapply() error {
	if !m.foreground() {
		return nil
	}
	return unix.IoctlSetTermios(m.fd, ioctlWriteTermios, &m.cbreak)
}

// restore sets the mode the terminal had before enterCbreakMode. It does
// nothing while dtail runs in the background: the shell owns the terminal
// then and sets its own mode.
func (m *cbreakTTY) restore() error {
	if !m.foreground() {
		return nil
	}
	return unix.IoctlSetTermios(m.fd, ioctlWriteTermios, &m.saved)
}

// foreground reports whether this process's group is the terminal's
// foreground process group. Setting the mode from a background job (after
// "bg", or when "kill %1" continues a stopped job to let it exit) raises
// SIGTTOU, whose default action stops the process instead. Ignoring
// SIGTTOU is no way out: the write would then change the shell's terminal.
// A job started in the background ("dtail &") thus gets cbreak mode only on
// its first SIGCONT ("fg").
func (m *cbreakTTY) foreground() bool {
	ttyPgrp, err := unix.IoctlGetInt(m.fd, unix.TIOCGPGRP)
	if err != nil {
		// Not this process's controlling terminal (e.g. a --control-tty of
		// another terminal): job control, and so SIGTTOU, does not apply.
		return true
	}
	// Getpgid(0) has one signature on every Unix (Getpgrp has not).
	ownPgrp, err := unix.Getpgid(0)
	return err == nil && ttyPgrp == ownPgrp
}

// watchTTYSignals calls onContinue when the process is continued after a
// stop (SIGCONT) and onResize when the terminal size changed (SIGWINCH),
// until stop is called.
func watchTTYSignals(onContinue, onResize func()) (stop func()) {
	signals := make(chan os.Signal, 4)
	gosignal.Notify(signals, syscall.SIGCONT, syscall.SIGWINCH)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case sig := <-signals:
				if sig == syscall.SIGCONT {
					onContinue()
				} else {
					onResize()
				}
			case <-done:
				return
			}
		}
	}()
	// stop waits for the watcher, so that no reapply runs after the caller
	// restored the terminal mode.
	return func() {
		gosignal.Stop(signals)
		close(done)
		<-finished
	}
}
