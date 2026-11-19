package integrationtests

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func TestDTailWithServer(t *testing.T) {
	testLogger := NewTestLogger("TestDTailWithServer")
	defer testLogger.WriteLogFile()
	cleanupTmpFiles(t)

	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	followFile := "dtail.follow.tmp"
	port := getUniquePortNumber()
	bindAddress := "localhost"
	greetings := []string{"World!", "Sol-System!", "Milky-Way!", "Universe!", "Multiverse!"}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	go func() {
		select {
		case <-time.After(time.Minute):
			t.Error("Max time for this test exceeded!")
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	serverCh, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	// MAYBETODO: In testmode, never read a config file (use none for all commands)
	clientCh, _, _, err := startCommand(ctx, t,
		"", "../dtail",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", followFile,
		"--grep", "Hello",
		"--trustAllHosts",
		"--noColor",
	)
	if err != nil {
		t.Error(err)
		return
	}
	// Write greetings to followFile
	fd, err := os.Create(followFile)
	if err != nil {
		t.Error(err)
	}
	defer fd.Close()

	go func() {
		var circular int
		for {
			select {
			case <-time.After(time.Second):
				_, _ = fd.WriteString(time.Now().String())
				_, _ = fd.WriteString(fmt.Sprintf(" - Hello %s\n", greetings[circular]))
				circular = (circular + 1) % len(greetings)
			case <-ctx.Done():
				return
			}
		}
	}()

	var greetingsRecv []string

readLoop:
	for len(greetingsRecv) < len(greetings) {
		select {
		case line := <-serverCh:
			t.Log("server:", line)
		case line := <-clientCh:
			t.Log("client:", line)
			if strings.Contains(line, "Hello ") {
				s := strings.Split(line, " ")
				greeting := s[len(s)-1]
				greetingsRecv = append(greetingsRecv, greeting)
				t.Log("Received greeting", greeting, len(greetingsRecv))
			}
		case <-ctx.Done():
			t.Log("Done reading client and server pipes")
			break readLoop
		}
	}

	// We expect to have received the greetings in the same order they were sent.`
	offset := -1
	for i, g := range greetings {
		if g == greetingsRecv[0] {
			offset = i
			break
		}
	}
	if offset == -1 {
		t.Error("Could not find first offset of greetings received")
		return
	}

	for i, g := range greetingsRecv {
		index := (i + offset) % len(greetings)
		if greetings[index] != g {
			t.Errorf("Expected '%s' but got '%s' at '%v' vs '%v'\n",
				g, greetings[index], greetings, greetingsRecv)
			return
		}
	}

	// File cleanup handled by cleanupTmpFiles
}

// TestDTailShutdownAfter is a regression guard for the follow-client shutdown
// bug (task 1v0): a `dtail --shutdownAfter N` follow session used to never
// return from client.Start, so the process hung until it was killed externally.
// With the fix the shutdownAfter deadline cancels the client context, which the
// reconnect and per-connection read loops honour, so the process exits shortly
// after N seconds. Pre-fix this test would block until the maxExit watchdog and
// fail; it must now pass well within that window.
func TestDTailShutdownAfter(t *testing.T) {
	testLogger := NewTestLogger("TestDTailShutdownAfter")
	defer testLogger.WriteLogFile()
	cleanupTmpFiles(t)

	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	const shutdownAfter = 3
	// Generous ceiling over shutdownAfter to absorb connection setup and
	// graceful teardown; the point is that the client returns at all, not that
	// it returns to the millisecond. Pre-fix it never returned.
	const maxExit = 15 * time.Second

	followFile := "dtail.shutdownafter.follow.tmp"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	// Overall safety net so a genuine hang cannot wedge the whole suite.
	go func() {
		select {
		case <-time.After(time.Minute):
			t.Error("Max time for this test exceeded!")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Start the server the follow client connects to.
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}
	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	// Keep appending to the followed file so the session has live work to do,
	// exercising the streaming read path rather than an idle connection.
	fd, err := os.Create(followFile)
	if err != nil {
		t.Error(err)
		return
	}
	defer fd.Close()
	go func() {
		for i := 0; ; i++ {
			select {
			case <-time.After(200 * time.Millisecond):
				_, _ = fd.WriteString(fmt.Sprintf("%s - Hello line %d\n", time.Now().String(), i))
			case <-ctx.Done():
				return
			}
		}
	}()

	// Run the follow client with --shutdownAfter and measure that it returns.
	cmd := exec.CommandContext(ctx, "../dtail",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", followFile,
		"--grep", "Hello",
		"--trustAllHosts",
		"--noColor",
		"--shutdownAfter", fmt.Sprintf("%d", shutdownAfter),
	)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Error(err)
		return
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-waitErr:
		elapsed := time.Since(start)
		t.Logf("dtail --shutdownAfter %d exited after %v (err=%v)", shutdownAfter, elapsed, err)
		// It must not exit meaningfully before the deadline (that would mean the
		// session failed rather than shut down on schedule).
		if elapsed < time.Duration(shutdownAfter)*time.Second-time.Second {
			t.Errorf("dtail exited after %v, before the %ds shutdownAfter deadline", elapsed, shutdownAfter)
		}
		if elapsed > maxExit {
			t.Errorf("dtail took %v to exit, want <= %v (follow shutdown regression)", elapsed, maxExit)
		}
	case <-time.After(maxExit):
		_ = cmd.Process.Kill()
		t.Errorf("dtail --shutdownAfter %d did not exit within %v: the follow client hung "+
			"(client.Start never returned)", shutdownAfter, maxExit)
	}
}

func TestDTailColorTable(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDTailColorTable")
	defer testLogger.WriteLogFile()

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		testDTailColorTableServerless(t, testLogger)
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		testDTailColorTableWithServer(t, testLogger)
	})
}

func testDTailColorTableServerless(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)

	outFile := "dtailcolortable.stdout.tmp"
	expectedOutFile := "dtailcolortable.expected"

	_, err := runCommand(ctx, t, outFile, "../dtail", "--colorTable")
	if err != nil {
		t.Error(err)
		return
	}
	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}

func testDTailColorTableWithServer(t *testing.T, logger *TestLogger) {
	outFile := "dtailcolortable.stdout.tmp"
	expectedOutFile := "dtailcolortable.expected"
	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	defer cancel()

	// Start dserver
	_, _, _, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Error(err)
		return
	}

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		t.Error(err)
		return
	}

	_, err = runCommand(ctx, t, outFile, "../dtail",
		"--colorTable",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--trustAllHosts")
	if err != nil {
		t.Error(err)
		return
	}

	cancel()

	if err := compareFilesWithContext(ctx, t, outFile, expectedOutFile); err != nil {
		t.Error(err)
		return
	}
}
