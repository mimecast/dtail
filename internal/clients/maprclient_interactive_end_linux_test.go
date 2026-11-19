//go:build linux

package clients

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
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
func waitForPTYOutput(t *testing.T, master *os.File, want string, done <-chan int) string {
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
	return output.String()
}

// TestInteractiveEndWhilePausedResumesOutput pauses the output of an
// --interactive-query dmap with an empty line on a real pseudo terminal and
// then ends the session with :quit or by closing the terminal. The control
// loop must resume the paused client logger when it ends, so that writers
// blocked by the pause finish and the client shuts down; Start returns 0.
func TestInteractiveEndWhilePausedResumesOutput(t *testing.T) {
	t.Parallel()

	for _, end := range []string{"quit", "control TTY end"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			master, ttyPath := openTestPTY(t)
			untilEnd := func(ctx context.Context) { <-ctx.Done() }
			c, _ := newSessionEndTestClient(t, untilEnd)
			logger := newPausingTestLogger()
			c.loggers.Client = logger
			c.InteractiveQuery = true
			c.ControlTTYPath = ttyPath

			done := make(chan int, 1)
			go func() { done <- c.Start(context.Background(), nil) }()
			waitForPTYOutput(t, master, "Press Enter to pause the output", done)
			if _, err := master.WriteString("\n"); err != nil {
				t.Fatalf("write empty line: %v", err)
			}
			waitForPTYOutput(t, master, interactivePausedMessage, done)
			if depth := logger.pauseDepth(); depth != 1 {
				t.Fatalf("pause depth = %d after the empty line, want 1", depth)
			}
			blocked := startWrite(func() { logger.Raw("payload") })
			requireBlocked(t, blocked)

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
				t.Fatal("paused interactive dmap did not end")
			}
			requireDone(t, blocked)
			if depth := logger.pauseDepth(); depth != 0 {
				t.Fatalf("pause depth = %d after the end, want 0", depth)
			}
		})
	}
}

// TestInteractiveEditorOnPTY runs the interactive control loop on a real
// pseudo terminal: the control TTY is switched to cbreak mode with signals
// and output processing kept, Tab completes commands, flags and the current
// value, the first key of a command pauses the output and the entered line
// resumes it, Up recalls the previous line, and after :quit the terminal
// mode is exactly what it was before.
func TestInteractiveEditorOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	termios := func() unix.Termios {
		t.Helper()
		tio, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatalf("get termios: %v", err)
		}
		return *tio
	}
	before := termios()

	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR|WARN"

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Tab completes", done)

	during := termios()
	if during.Lflag&(unix.ICANON|unix.ECHO) != 0 || during.Lflag&unix.ISIG == 0 || during.Oflag&unix.OPOST == 0 {
		t.Fatalf("control TTY mode lflag %#x oflag %#x: want no ICANON/ECHO, ISIG and OPOST kept",
			during.Lflag, during.Oflag)
	}

	write := func(s string) {
		t.Helper()
		if _, err := master.WriteString(s); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	write(":help\n") // The first key shows the hint; :help keeps the output paused.
	waitForPTYOutput(t, master, interactiveTypingHint, done)
	waitForPTYOutput(t, master, interactivePausedMessage, done)
	if depth := logger.pauseDepth(); depth != 1 {
		t.Fatalf("pause depth after :help = %d, want 1 until the next empty line", depth)
	}
	write("\n")
	waitForPTYOutput(t, master, interactiveResumedMessage, done)
	waitForPauseDepth(t, logger, 0)
	write(":\t") // Ambiguous: Tab lists the commands.
	waitForPTYOutput(t, master, ":reload  :show  :fields  :probe  :help  :quit", done)
	write("re\t")
	waitForPTYOutput(t, master, ":reload ", done)
	if depth := logger.pauseDepth(); depth != 1 {
		t.Fatalf("pause depth while typing = %d, want 1", depth)
	}
	write("--gr\t")
	waitForPTYOutput(t, master, "--grep ", done)
	write("\t")
	waitForPTYOutput(t, master, "'ERROR|WARN'", done)
	write("\x15:show\n") // Ctrl-U clears the line first.
	waitForPTYOutput(t, master, "mode=grep", done)
	waitForPauseDepth(t, logger, 0)
	write("\x1b[A") // Up recalls :show and pauses the output for typing.
	waitForPTYOutput(t, master, ":show", done)
	waitForPauseDepth(t, logger, 1)
	write("\n")
	waitForPTYOutput(t, master, "mode=grep", done)
	waitForPauseDepth(t, logger, 0)
	// A line the editor may have cut is rejected, not run, and resumes the
	// output its keys paused.
	write(strings.Repeat("x", editorMaxLine+10) + "\n")
	waitForPTYOutput(t, master, "line too long", done)
	waitForPauseDepth(t, logger, 0)
	write(":quit\n")

	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("Start status = %d, want 0", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive client did not end")
	}
	after := termios()
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag || after.Oflag != before.Oflag ||
		after.Cc != before.Cc {
		t.Fatalf("terminal mode not restored: before %+v after %+v", before, after)
	}
	if depth := logger.pauseDepth(); depth != 0 {
		t.Fatalf("pause depth after the end = %d, want 0", depth)
	}
}

// waitForPauseDepth waits until the logger's pause depth is want: the
// control loop resumes right after it wrote the command's answer.
func waitForPauseDepth(t *testing.T, logger *pausingTestLogger, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for logger.pauseDepth() != want {
		if time.Now().After(deadline) {
			t.Fatalf("pause depth %d, want %d", logger.pauseDepth(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ptyTermios reads the termios of the pseudo terminal slave at path.
func ptyTermios(t *testing.T, path string) unix.Termios {
	t.Helper()
	slave, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = slave.Close() }()
	tio, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios: %v", err)
	}
	return *tio
}

func sameTermios(a, b unix.Termios) bool {
	return a.Lflag == b.Lflag && a.Iflag == b.Iflag && a.Oflag == b.Oflag && a.Cc == b.Cc
}

// TestInteractiveEditorRestoresOnCancel cancels an interactive session while
// a command is being typed: the terminal mode is restored and the typing
// pause released, as on :quit.
func TestInteractiveEditorRestoresOnCancel(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	before := ptyTermios(t, ttyPath)
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- c.Start(ctx, nil) }()
	waitForPTYOutput(t, master, "Tab completes", done)
	if _, err := master.WriteString(":re"); err != nil {
		t.Fatal(err)
	}
	waitForPauseDepth(t, logger, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interactive client did not end on cancel")
	}
	if after := ptyTermios(t, ttyPath); !sameTermios(before, after) {
		t.Fatalf("terminal mode not restored on cancel: before %+v after %+v", before, after)
	}
	if depth := logger.pauseDepth(); depth != 0 {
		t.Fatalf("pause depth after cancel = %d, want 0", depth)
	}
}

// TestInteractiveEditorForcedExitAndContinue checks, on a real pseudo
// terminal, that the editor registers its restore for the forced exit (which
// skips deferred calls) and clears it on close, and that SIGCONT (after
// Ctrl-Z and fg, which bash and zsh answer with their cooked mode) sets
// cbreak mode again.
func TestInteractiveEditorForcedExitAndContinue(t *testing.T) {
	t.Parallel()
	_, ttyPath := openTestPTY(t)
	before := ptyTermios(t, ttyPath)
	var hook func()
	hookSet := false

	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	client := newPauseTestClient(newPausingTestLogger())
	deps := testEditorDeps()
	deps.watchSignals = watchTTYSignals // SIGCONT is what this tests.
	deps.setForceExitHook = func(f func()) { hook, hookSet = f, true }
	lines := client.newControlLines(slave, client.newInteractivePause(), nil, deps)
	if _, ok := lines.(*interactiveEditor); !ok {
		t.Fatalf("control lines on a terminal = %T, want *interactiveEditor", lines)
	}
	if !hookSet || hook == nil {
		t.Fatal("editor registered no restore for the forced exit")
	}
	cbreak := ptyTermios(t, ttyPath)
	if cbreak.Lflag&unix.ICANON != 0 {
		t.Fatal("editor did not set cbreak mode")
	}

	// A shell's "fg" leaves the terminal cooked; SIGCONT re-applies cbreak.
	cooked := before
	if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, &cooked); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ptyTermios(t, ttyPath).Lflag&unix.ICANON != 0 {
		if time.Now().After(deadline) {
			t.Fatal("cbreak mode not re-applied after SIGCONT")
		}
		time.Sleep(10 * time.Millisecond)
	}

	hook() // what the forced exit runs
	if after := ptyTermios(t, ttyPath); !sameTermios(before, after) {
		t.Fatalf("forced-exit hook did not restore: before %+v after %+v", before, after)
	}
	lines.close()
	if hook != nil {
		t.Fatal("close did not clear the forced-exit hook")
	}
}

// TestInteractiveEditorForcedExitWhileWatcherBlocked checks that the
// forced-exit hook restores the terminal mode and returns while the
// SIGCONT/SIGWINCH watcher is blocked: stopped output (Ctrl-S, here TCOOFF)
// blocks a Terminal.Write that holds x/term's lock, and a resize then blocks
// the watcher in Terminal.SetSize. The normal close waits for the watcher,
// so the hook must not. Not parallel: it sends SIGWINCH to the process.
func TestInteractiveEditorForcedExitWhileWatcherBlocked(t *testing.T) {
	_, ttyPath := openTestPTY(t)
	before := ptyTermios(t, ttyPath)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	slaveFd := int(slave.Fd())
	// A window size, so that a resize calls Terminal.SetSize.
	if err := unix.IoctlSetWinsize(slaveFd, unix.TIOCSWINSZ,
		&unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Skipf("set window size: %v", err)
	}
	var hook func()
	client := newPauseTestClient(newPausingTestLogger())
	deps := testEditorDeps()
	deps.watchSignals = watchTTYSignals // SIGWINCH is what this tests.
	deps.setForceExitHook = func(f func()) { hook = f }
	lines := client.newControlLines(slave, client.newInteractivePause(), nil, deps)
	editor, ok := lines.(*interactiveEditor)
	if !ok || hook == nil {
		t.Fatalf("control lines on a terminal = %T (hook set %v), want *interactiveEditor with hook",
			lines, hook != nil)
	}
	if err := unix.IoctlSetInt(slaveFd, unix.TCXONC, unix.TCOOFF); err != nil {
		t.Skipf("stop terminal output: %v", err)
	}
	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = editor.terminal.Write([]byte("blocked on the stopped terminal\n"))
	}()
	waitForGoroutineIn(t, "(*Terminal).Write")
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	waitForGoroutineIn(t, "(*Terminal).SetSize")

	hookDone := make(chan struct{})
	go func() { hook(); close(hookDone) }()
	select {
	case <-hookDone:
	case <-time.After(5 * time.Second):
		t.Fatal("forced-exit hook blocked behind the SIGCONT/SIGWINCH watcher")
	}
	if after := ptyTermios(t, ttyPath); !sameTermios(before, after) {
		t.Fatalf("forced-exit hook did not restore: before %+v after %+v", before, after)
	}

	// Restart the output: the write, the watcher and the normal close end.
	if err := unix.IoctlSetInt(slaveFd, unix.TCXONC, unix.TCOON); err != nil {
		t.Fatal(err)
	}
	<-written
	lines.close()
}

// waitForGoroutineIn waits up to 5 s until the stack of a goroutine
// contains frame.
func waitForGoroutineIn(t *testing.T, frame string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1<<20)
	for !strings.Contains(string(buf[:runtime.Stack(buf, true)]), frame) {
		if time.Now().After(deadline) {
			t.Fatalf("no goroutine in %s within 5s", frame)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestInteractiveEditorCompletesQueryOnPTY presses Tab inside a --query value
// on a real pseudo terminal: a keyword completes inside the open single
// quote, and where only field data could help, the hint says that no field
// data arrived (the test session has no servers to probe).
func TestInteractiveEditorCompletesQueryOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	c.loggers.Client = newPausingTestLogger()
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.MapClient
	c.What = "/var/log/app.log"
	// The session reads the file (a session without files gets the
	// no-files hint instead, TestInteractiveEditorNoFilesOnPTY).
	c.sessionSpec.Files = []string{c.What}
	c.QueryStr = "from STATS select count($line) group by $hostname"

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Tab completes", done)
	write := func(s string) {
		t.Helper()
		if _, err := master.WriteString(s); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	write(":reload --query 'sel\t")
	waitForPTYOutput(t, master, "'select ", done)
	write("\t")
	waitForPTYOutput(t, master, queryCatalogHint, done)
	write("\x15:quit\n")
	awaitPTYClientEnd(t, done)
}

// TestInteractiveEditorNoFilesOnPTY starts a session that reads no files on
// a real pseudo terminal: the banner is followed by the hint how to give it
// files, and :fields and :probe answer with it instead of waiting for field
// data that can never arrive.
func TestInteractiveEditorNoFilesOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	c.loggers.Client = newPausingTestLogger()
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.MapClient
	c.What = ""
	c.sessionSpec.Files = nil

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	hint := sessionNoFilesText + sessionNoFilesRemoteTab
	// One wait for both: the helper drops what it read past a match.
	waitForPTYOutput(t, master, hint, done)
	if _, err := master.WriteString(":fields\n"); err != nil {
		t.Fatal(err)
	}
	// Only the answer of :fields is followed by the pause message: typing
	// paused the output, and :fields keeps it paused.
	waitForPTYOutput(t, master, hint+"\r\r\n"+interactivePausedMessage, done)
	if _, err := master.WriteString(":probe\n"); err != nil {
		t.Fatal(err)
	}
	waitForPTYOutput(t, master, "nothing to probe: "+hint, done)
	if _, err := master.WriteString(":quit\n"); err != nil {
		t.Fatal(err)
	}
	awaitPTYClientEnd(t, done)
}

// awaitPTYClientEnd waits for the interactive client of a PTY test to end
// with status 0.
func awaitPTYClientEnd(t *testing.T, done <-chan int) {
	t.Helper()
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("Start status = %d, want 0", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive client did not end")
	}
}

// TestInteractiveEditorColorsOnPTY runs the line editor with terminal colors
// enabled: control messages and candidate lists are painted, and a line
// started while the output is paused shows the prompt, one started while
// it runs none.
func TestInteractiveEditorColorsOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	c.runtime = &clientRuntimeBoundary{clientCfg: &config.ClientConfig{TermColorsEnable: true,
		TermColors: config.DefaultTermColors()}}
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR"
	style := c.interactiveStyle()
	if style == nil {
		t.Fatal("no style with colors enabled")
	}

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, style.banner(interactiveBanner), done)
	write := func(s string) {
		t.Helper()
		if _, err := master.WriteString(s); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	write(":")
	waitForPTYOutput(t, master, style.hint(interactiveTypingHint), done)
	write("\t")
	waitForPTYOutput(t, master, style.paint(style.theme.Command, ":reload"), done)
	write("bogus\n")
	waitForPTYOutput(t, master, style.failure(`interactive query error: unknown command ":bogus"`), done)
	waitForPauseDepth(t, logger, 0)
	// An explicit pause prints the help, and the next line shows the
	// prompt. The helper drops what it read past a match, so the end of an
	// answer and the prompt drawn right after it are waited for as one.
	write("\n")
	waitForPTYOutput(t, master, "interactive-query.md).\r\r\n"+style.prompt(), done)
	write(":show\n") // Answered while paused: the next line shows the prompt again.
	waitForPTYOutput(t, master, style.paint(style.theme.Key, "servers")+"="+
		style.paint(style.theme.Value, "1")+" "+style.paint(style.theme.Key, "sessions")+"="+
		style.paint(style.theme.Value, "0")+"\r\r\n"+style.prompt(), done)
	// Resumed: the next line has no prompt. Its first key pauses for typing,
	// and the Terminal, at column 0 with no prompt drawn, writes the hint
	// without a redraw, so the key follows the hint directly.
	write("\n")
	waitForPTYOutput(t, master, style.paused(interactiveResumedMessage)+"\r\r\n", done)
	write(":")
	waitForPTYOutput(t, master, style.hint(interactiveTypingHint)+"\r\r\n:", done)
	write("quit\n")
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("Start status = %d, want 0", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive client did not end")
	}
}
