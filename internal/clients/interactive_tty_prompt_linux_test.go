//go:build linux

package clients

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
	"golang.org/x/sys/unix"
)

// TestInteractiveEditorWhileHostKeyQuestionOnPTY runs the control loop on a
// real pseudo terminal while the host key question is asked: the keys of the
// answer still pause the output, but print no typing hint (it would land
// right behind the question and push the answer to a later row), Tab
// completes nothing, and the answer does not enter the history. Once the
// question is answered, a command shows the hint as usual.
func TestInteractiveEditorWhileHostKeyQuestionOnPTY(t *testing.T) {
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
	router := &fakePromptRouter{}
	answers := &promptAnswers{router: router, lines: make(chan []byte, 16)}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() {
		_ = c.runInteractiveControl(ctx, cancel, slave, answers)
		done <- 0
	}()
	waitForPTYOutput(t, master, "Tab completes", done)

	router.prompting.Store(true)
	writePTY(t, master, ":rel")
	waitForPauseDepth(t, logger, 1) // The answer's keys pause the output.
	writePTY(t, master, "\t\n")
	select {
	case answer := <-answers.lines:
		if string(answer) != ":rel\n" {
			t.Fatalf("answer = %q, want %q (Tab must not complete it)", answer, ":rel\n")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the answer did not reach the prompt")
	}
	waitForPauseDepth(t, logger, 0)
	router.prompting.Store(false)

	writePTY(t, master, ":show\n")
	out := waitForPTYOutput(t, master, "mode=grep", done)
	if n := strings.Count(out, interactiveTypingHint); n != 1 {
		t.Fatalf("typing hint shown %d times, want once (for :show only, not the answer): %q", n, out)
	}
	waitForPauseDepth(t, logger, 0)
	// Up twice: the answer, were it in the history, would be recalled and
	// run as an unknown command; without it the line stays :show.
	writePTY(t, master, "\x1b[A\x1b[A\n")
	out = waitForPTYOutput(t, master, "mode=grep", done)
	if strings.Contains(out, "unknown") {
		t.Fatalf("the answer was recalled from the history: %q", out)
	}
	cancel()
	<-done
}

// TestInteractiveEditorPlainPromptOnNarrowPTY pauses the output on a real
// pseudo terminal 24 columns wide, narrower than the painted prompt has
// runes (x/term cuts it into rows by runes, escape sequences included):
// the line shows the plain prompt, and a typed key is echoed right behind
// it without a cursor correction (an extra ESC[D was the miscount).
func TestInteractiveEditorPlainPromptOnNarrowPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Row: 30, Col: 24}); err != nil {
		t.Skipf("set window size: %v", err)
	}
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	c.loggers.Client = newPausingTestLogger()
	c.runtime = &clientRuntimeBoundary{clientCfg: &config.ClientConfig{TermColorsEnable: true,
		TermColors: config.DefaultTermColors()}}
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR"

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Tab completes", done)
	writePTY(t, master, "\n")
	waitForPTYOutput(t, master, "interactive-query.md).\r\r\n"+interactivePrompt, done)
	writePTY(t, master, ":")
	if out := waitForPTYOutput(t, master, ":", done); strings.Contains(out, "\x1b[D") {
		t.Fatalf("the cursor was corrected after the first key (prompt miscounted): %q", out)
	}
	writePTY(t, master, "quit\n")
	requireStartStatus(t, done, 0)
}

// TestInteractiveEditorRedrawsAfterInterruptStatsOnPTY presses Ctrl-C (a
// message on statsCh) while a command is being typed: the statistics are
// written through the line editor, which takes the edit line off the
// screen, writes them and draws the line again below them, so the line is
// not overwritten and the Terminal's cursor bookkeeping stays right.
func TestInteractiveEditorRedrawsAfterInterruptStatsOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	c.stats = newTailStats(1, nil, 10*time.Millisecond, nil)
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR"

	statsCh := make(chan string, 1)
	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), statsCh) }()
	waitForPTYOutput(t, master, "Tab completes", done)
	writePTY(t, master, ":sh")
	waitForPauseDepth(t, logger, 1)
	waitForPTYOutput(t, master, ":sh", done)

	statsCh <- "Hint: Hit Ctrl+C again to exit"
	// Each message line is one Terminal.Write: the line (3 columns) is
	// cleared, the message written (OPOST adds a CR to its CRLF) and the
	// line drawn again below it, before the next message clears it again.
	waitForPTYOutput(t, master,
		"\x1b[3D\x1b[K Hint: Hit Ctrl+C again to exit\r\r\n:sh\x1b[3D\x1b[K Connection stats: ", done)
	writePTY(t, master, "ow\n") // The line is still :sh, completed to :show.
	waitForPTYOutput(t, master, "mode=grep", done)
	writePTY(t, master, ":quit\n")
	requireStartStatus(t, done, 0)
}
