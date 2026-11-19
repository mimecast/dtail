//go:build linux

package clients

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mimecast/dtail/internal/omode"
)

// ptySettle is how long readPTYSettled reads: long enough for a redraw
// that should not happen to arrive.
const ptySettle = 300 * time.Millisecond

// readPTYSettled returns what the pseudo terminal master gets within
// ptySettle.
func readPTYSettled(t *testing.T, master *os.File) string {
	t.Helper()
	defer func() { _ = master.SetReadDeadline(time.Time{}) }()
	if err := master.SetReadDeadline(time.Now().Add(ptySettle)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var output strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := master.Read(buf)
		output.Write(buf[:n])
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return output.String()
		}
		if err != nil {
			t.Fatalf("read control TTY output %q: %v", output.String(), err)
		}
	}
}

// TestInteractiveEditorBracketedPasteOnPTY runs the control loop on a real
// pseudo terminal and pastes as a terminal in bracketed paste mode does,
// which the editor asks for at its start and gives back at its end. Pasted
// text that stays on the line pauses the output like typed text, and its
// tab completes nothing: it becomes a blank. The lines of a multi-line
// paste arrive as lines, each run as entered, and its blank lines are
// skipped: none toggles the output pause as the Enter key does.
func TestInteractiveEditorBracketedPasteOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() {
		_ = c.runInteractiveControl(ctx, cancel, slave, nil, testEditorDeps())
		done <- 0
	}()
	start := waitForPTYOutput(t, master, "Tab completes", done)
	if !strings.Contains(start, pasteModeOn) {
		t.Fatalf("editor did not ask for bracketed paste: %q", start)
	}

	// Typed, ":sh" plus Tab completes to ":show"; pasted, the tab is text.
	writePTY(t, master, "\x1b[200~:sh\tow\x1b[201~")
	out := waitForPTYOutput(t, master, ":sh ow", done)
	if !strings.Contains(out, interactiveTypingHint) || strings.Contains(out, "\t") {
		t.Fatalf("paste on the line: %q, want the typing hint and the tab as a blank", out)
	}
	waitForPauseDepth(t, logger, 1)
	writePTY(t, master, "\x15\n") // An empty line ends the typing pause.
	waitForPauseDepth(t, logger, 0)

	writePTY(t, master, "\x1b[200~\n:show\n\n  \n:quit\n\x1b[201~")
	out = waitForPTYOutput(t, master, "quitting interactive session", nil)
	if !strings.Contains(out, "mode=grep") {
		t.Fatalf("multi-line paste: %q, want :show run before :quit", out)
	}
	if strings.Contains(out, interactivePausedMessage) || strings.Contains(out, interactiveResumedMessage) {
		t.Fatalf("a blank line of the paste toggled the output pause: %q", out)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("control loop did not end after the pasted :quit")
	}
	if end := readPTYSettled(t, master); !strings.Contains(out+end, pasteModeOff) {
		t.Fatalf("editor left bracketed paste on at its end: %q", out+end)
	}
}

// TestInteractiveEditorHiddenLineIsNotRedrawn takes the line being edited
// off a real pseudo terminal as a Tab's wait does and, while it is hidden,
// writes control output from another goroutine (the Ctrl-C statistics) and
// resizes the window (SIGWINCH): neither may have the Terminal draw, as it
// would redraw the line from the cursor position it remembers. The
// statistics are written as they are and the resize is taken over when the
// line is shown again.
func TestInteractiveEditorHiddenLineIsNotRedrawn(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	// The slave's descriptor: Fd on the master would make it blocking, and
	// its read deadlines would no longer hold.
	setWinsize := func(columns uint16) {
		t.Helper()
		size, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", ttyPath, err)
		}
		defer func() { _ = size.Close() }()
		if err := unix.IoctlSetWinsize(int(size.Fd()), unix.TIOCSWINSZ,
			&unix.Winsize{Row: 30, Col: columns}); err != nil {
			t.Skipf("set window size: %v", err)
		}
	}
	setWinsize(100)
	client := newPauseTestClient(newPausingTestLogger())
	lines := client.newControlLines(slave, client.newInteractivePause(), nil, testEditorDeps())
	editor, ok := lines.(*interactiveEditor)
	if !ok {
		t.Fatalf("control lines on a terminal = %T, want *interactiveEditor", lines)
	}
	read := make(chan string, 1)
	go func() {
		line, _ := lines.ReadLine()
		read <- line
	}()
	const typed = ":reload --regex typed-so-far"
	writePTY(t, master, typed)
	waitForPTYOutput(t, master, typed, nil)

	editor.hideLine(typed, len(typed))
	setWinsize(60)
	editor.resize() // what the SIGWINCH watcher calls
	if _, err := lines.Writer().Write([]byte("STATS while hidden\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := readPTYSettled(t, master), "\r\x1b[JSTATS while hidden\r\n"; got != want {
		t.Fatalf("while the line was hidden the TTY got %q, want only the erased line and the statistics %q",
			got, want)
	}
	if columns := editor.screen.columns(); columns != 100 {
		t.Fatalf("resize applied while the line was hidden: %d columns, want it deferred (100)", columns)
	}

	editor.showLine(typed, len(typed))
	if got := readPTYSettled(t, master); !strings.Contains(got, typed) {
		t.Fatalf("line not drawn again after the wait: %q", got)
	}
	if columns := editor.screen.columns(); columns != 60 {
		t.Fatalf("deferred resize not applied after the wait: %d columns, want 60", columns)
	}
	// Shown again, control output goes through the Terminal, which draws
	// the line below it.
	if _, err := lines.Writer().Write([]byte("STATS while shown\n")); err != nil {
		t.Fatal(err)
	}
	got := readPTYSettled(t, master)
	stats := strings.Index(got, "STATS while shown")
	if stats < 0 || !strings.Contains(got[stats:], typed) || !strings.HasPrefix(got, "\x1b[") {
		t.Fatalf("control output with the line shown: %q, want the line cleared and drawn again below it", got)
	}
	writePTY(t, master, "\n")
	select {
	case line := <-read:
		if line != typed {
			t.Fatalf("line read = %q, want %q", line, typed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("line not read")
	}
	lines.close()
}
