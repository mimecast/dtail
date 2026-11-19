//go:build linux

package clients

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// TestInteractiveEditorAltEOnPTY runs the control loop on a real pseudo
// terminal and opens the external editor with Alt-E: the editor gets the
// line typed so far, runs with the terminal in the mode dtail found it in
// (canonical, echo), and its text replaces the line, which runs only on
// Enter. An editor that fails keeps the line and says why.
func TestInteractiveEditorAltEOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	dir := t.TempDir()
	editor := filepath.Join(dir, "editor")
	failing := filepath.Join(dir, "failing")
	writeScript(t, editor, `cp "$1" "`+dir+`/given"; stty -a > "`+dir+
		`/mode"; printf ':show\n' > "$1"`)
	writeScript(t, failing, "exit 3")
	var command atomic.Value
	command.Store(editor)

	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	c.loggers.Client = newPausingTestLogger()
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR"
	deps := testEditorDeps()
	deps.getenv = func(name string) string {
		if name == "EDITOR" {
			return command.Load().(string)
		}
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() {
		_ = c.runInteractiveControl(ctx, cancel, slave, nil, deps)
		done <- 0
	}()
	waitForPTYOutput(t, master, "Tab completes", done)

	writePTY(t, master, ":sh\x1be")
	waitForPTYOutput(t, master, ":show", done)
	given, err := os.ReadFile(filepath.Join(dir, "given"))
	if err != nil || string(given) != ":sh\n" {
		t.Fatalf("editor got %q (%v), want %q", given, err, ":sh\n")
	}
	mode, err := os.ReadFile(filepath.Join(dir, "mode"))
	if err != nil || strings.Contains(string(mode), "-icanon") ||
		!strings.Contains(string(mode), "icanon") {
		t.Fatalf("editor ran without canonical mode: %q (%v)", mode, err)
	}
	writePTY(t, master, "\n")
	waitForPTYOutput(t, master, "mode=grep", done)

	command.Store(failing)
	writePTY(t, master, ":hel\x1bv")
	out := waitForPTYOutput(t, master, "line kept", done)
	if !strings.Contains(out, "exit status 3") {
		t.Fatalf("failing editor not reported: %q", out)
	}
	writePTY(t, master, "p\n")
	waitForPTYOutput(t, master, ":reload", done)
	cancel()
	<-done
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestInteractiveEditorViModeOnPTY runs the control loop on a real pseudo
// terminal with Client.EditMode "vi": a lone Esc enters normal mode (block
// cursor), b and D delete the last word, Enter runs the line from normal
// mode, the next line starts in insert mode again (bar cursor), and k in
// normal mode recalls the history.
func TestInteractiveEditorViModeOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	untilEnd := func(ctx context.Context) { <-ctx.Done() }
	c, _ := newSessionEndTestClient(t, untilEnd)
	c.loggers.Client = newPausingTestLogger()
	c.runtime = &clientRuntimeBoundary{clientCfg: &config.ClientConfig{EditMode: config.EditModeVi}}
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
	waitForPTYOutput(t, master, "Tab completes", done)

	// Esc alone in one write, as a terminal sends the key.
	writePTY(t, master, ":show junk")
	waitForPTYOutput(t, master, "junk", done)
	writePTY(t, master, "\x1b")
	waitForPTYOutput(t, master, viCursorShapes[viNormal], done)
	writePTY(t, master, "bD\n")
	out := waitForPTYOutput(t, master, "mode=grep", done)
	if strings.Contains(out, "unknown") {
		t.Fatalf("the edited line did not run as :show: %q", out)
	}
	// The next line's insert mode cursor may come with the same read.
	if _, after, _ := strings.Cut(out, "mode=grep"); !strings.Contains(after, viCursorShapes[viInsert]) {
		waitForPTYOutput(t, master, viCursorShapes[viInsert], done)
	}

	writePTY(t, master, "\x1b")
	waitForPTYOutput(t, master, viCursorShapes[viNormal], done)
	writePTY(t, master, "k\n")
	waitForPTYOutput(t, master, "mode=grep", done)
	cancel()
	<-done
	if out := drainPTY(master); !strings.Contains(out, viCursorDefault) {
		t.Fatalf("the cursor shape was not given back at the end: %q", out)
	}
}

// drainPTY returns what the master side of a pseudo terminal still has to
// read, reading until nothing arrives for half a second.
func drainPTY(master *os.File) string {
	defer func() { _ = master.SetReadDeadline(time.Time{}) }()
	var out strings.Builder
	buf := make([]byte, 1024)
	for {
		_ = master.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := master.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			return out.String()
		}
	}
}
