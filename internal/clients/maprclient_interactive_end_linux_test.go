//go:build linux

package clients

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMaprClientInteractiveEndIsNoCutShort runs an --interactive-query dmap
// on a real pseudo terminal as its control TTY and ends it with :quit or by
// closing the terminal. Both cancel only the context of runConnections, not
// that of Start, so the sessions they end (which end without the server's
// close handshake) must count as ended by the client: no warning that the
// result may be incomplete, and the exit status stays 0.
func TestMaprClientInteractiveEndIsNoCutShort(t *testing.T) {
	t.Parallel()

	for _, end := range []string{"quit", "control TTY end"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			master, ttyPath := openTestPTY(t)
			untilEnd := func(ctx context.Context) { <-ctx.Done() }
			c, logger := newSessionEndTestClient(t, untilEnd, untilEnd)
			c.InteractiveQuery = true
			c.ControlTTYPath = ttyPath

			done := make(chan int, 1)
			go func() { done <- c.Start(context.Background(), nil) }()
			waitForPTYOutput(t, master, "Interactive query control enabled", done)
			if end == "quit" {
				if _, err := master.WriteString(":quit\n"); err != nil {
					t.Fatalf("write :quit: %v", err)
				}
			} else if err := master.Close(); err != nil {
				t.Fatalf("close control TTY: %v", err)
			}

			select {
			case status := <-done:
				if status != 0 {
					t.Fatalf("Start status = %d, want 0", status)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("interactive dmap did not end")
			}
			if got := incompleteWarnings(logger); len(got) != 0 {
				t.Fatalf("warnings = %q, want none", got)
			}
		})
	}
}

// openTestPTY opens a new pseudo terminal and returns its master side
// (closed when the test ends) and the path of its slave side, which the
// client opens as its control TTY. It skips the test where no pseudo
// terminal can be opened, including when the slave path cannot be opened
// (e.g. a container whose /dev/pts is not the devpts instance /dev/ptmx
// belongs to), so such an environment never fails in the client instead.
func openTestPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	raw, err := master.SyscallConn()
	if err != nil {
		t.Fatalf("pseudo terminal: %v", err)
	}
	var number uint32
	ctlErr := raw.Control(func(fd uintptr) {
		// Unlock the slave side, then ask for its number. Control, unlike
		// Fd, keeps the master non-blocking, so Close ends a pending Read.
		if err = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); err == nil {
			number, err = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
		}
	})
	if ctlErr != nil || err != nil {
		t.Skipf("pseudo terminal setup: %v %v", ctlErr, err)
	}
	path := fmt.Sprintf("/dev/pts/%d", number)
	slave, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave %s: %v", path, err)
	}
	// Keep this probe open until the test ends: closing the last open slave
	// makes reads of the master fail with EIO until the client opens the
	// slave, which would race with waitForPTYOutput.
	t.Cleanup(func() { _ = slave.Close() })
	return master, path
}

// waitForPTYOutput reads the master side of a pseudo terminal until the
// output contains want, so that the client opened its control TTY and runs
// its control loop. It fails at once should the client's Start return
// (reported on done) before that, instead of waiting for the deadline.
func waitForPTYOutput(t *testing.T, master *os.File, want string, done <-chan int) {
	t.Helper()
	defer func() { _ = master.SetReadDeadline(time.Time{}) }()
	deadline := time.Now().Add(10 * time.Second)
	var output strings.Builder
	buf := make([]byte, 1024)
	for !strings.Contains(output.String(), want) {
		select {
		case status := <-done:
			t.Fatalf("Start returned %d before the control TTY output %q (got %q)",
				status, want, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no control TTY output %q within 10s (got %q)", want, output.String())
		}
		// Read in short slices so that an early return of Start is noticed.
		if err := master.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		n, err := master.Read(buf)
		output.Write(buf[:n])
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("read control TTY output %q: %v", output.String(), err)
		}
	}
}
