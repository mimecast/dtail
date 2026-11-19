//go:build linux

package clients

import (
	"os"
	"syscall"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

// TestInteractiveEditorRegistersForcedExitHookLast checks, on a real pseudo
// terminal, that the editor registers its restore for the forced exit only
// once it is complete: a forced exit may run the hook the moment it is
// registered, and the hook reads the editor's vi layer and screen to give
// the terminal its default cursor shape back. Run at registration, it must
// therefore write that shape and restore the terminal mode.
func TestInteractiveEditorRegistersForcedExitHookLast(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	before := ptyTermios(t, ttyPath)
	slave, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo terminal slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	client := newPauseTestClient(newPausingTestLogger())
	client.runtime.clientCfg = &config.ClientConfig{EditMode: config.EditModeVi}
	registered := 0
	deps := testEditorDeps()
	deps.setForceExitHook = func(hook func()) {
		if hook != nil {
			registered++
			hook() // the forced exit, as early as it can run
		}
	}
	lines := client.newControlLines(slave, client.newInteractivePause(), nil, deps)
	editor, ok := lines.(*interactiveEditor)
	if !ok || registered != 1 {
		t.Fatalf("control lines on a terminal = %T, hook registered %d times, want *interactiveEditor and once",
			lines, registered)
	}
	if editor.vi == nil || editor.stop == nil || editor.resize == nil {
		t.Fatal("editor returned incomplete")
	}
	// Before close, which writes the default shape as well.
	waitForPTYOutput(t, master, viCursorDefault, nil)
	if after := ptyTermios(t, ttyPath); !sameTermios(before, after) {
		t.Fatalf("hook run at registration did not restore: before %+v after %+v", before, after)
	}
	lines.close()
}
