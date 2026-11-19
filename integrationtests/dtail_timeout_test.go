package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// TestDTailTimeoutExits guards the client-side --timeout deadline for the dtail
// follow client (task xu0). Historically the server-side read deadline fired at
// N seconds, but in tail+query streaming mode the session stayed alive via the
// map/aggregate command, so the client treated the closed read as a transient
// drop and auto-reconnected for another N-second cycle indefinitely - the
// process never exited. Plain "dtail --timeout N" (no --query) never emitted the
// timeout at all. Both are now covered by a client-side context deadline in
// cmd/dtail/main.go: whichever of --timeout / --shutdownAfter elapses first
// cancels the client context, so client.Start returns and the process exits.
//
// The assertion is that dtail exits well within a generous guard window. Without
// the fix the reconnect loop keeps the process alive past the guard, failing the
// test.
func TestDTailTimeoutExits(t *testing.T) {
	testLogger := NewTestLogger("TestDTailTimeoutExits")
	defer testLogger.WriteLogFile()
	cleanupTmpFiles(t)

	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	t.Run("QueryTimeoutExits", func(t *testing.T) {
		runDTailTimeoutCase(t, testLogger, "dtail.timeout.query.tmp",
			[]string{"--query", "select count($line) as cnt from STATS"})
	})
	t.Run("PlainTimeoutExits", func(t *testing.T) {
		runDTailTimeoutCase(t, testLogger, "dtail.timeout.plain.tmp", nil)
	})
}

// runDTailTimeoutCase starts a dserver, follows a growing file with
// "dtail --timeout <timeoutSeconds>" (plus any extra args), and asserts the
// client process exits within guardSeconds. The timeout is short relative to the
// guard so a regression (reconnect-after-timeout hang or a silently ignored
// --timeout) is caught by the guard firing first.
func runDTailTimeoutCase(t *testing.T, testLogger *TestLogger, followFile string, extraArgs []string) {
	const (
		timeoutSeconds = 3
		guardSeconds   = 20
	)

	port := getUniquePortNumber()
	bindAddress := "localhost"

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	if err := startTestServer(ctx, t, &ServerConfig{
		Port:        port,
		BindAddress: bindAddress,
		LogLevel:    "error",
	}); err != nil {
		t.Fatalf("unable to start dserver: %v", err)
	}

	// Keep the file growing so the follow stays active (and would keep
	// reconnecting without the client-side deadline) until the timeout fires.
	fd, err := os.Create(followFile)
	if err != nil {
		t.Fatalf("unable to create follow file: %v", err)
	}
	defer func() {
		_ = fd.Close()
		_ = os.Remove(followFile)
	}()
	go func() {
		for i := 0; ; i++ {
			select {
			case <-time.After(200 * time.Millisecond):
				_, _ = fd.WriteString(fmt.Sprintf("%s Hello line %d\n", time.Now(), i))
			case <-ctx.Done():
				return
			}
		}
	}()

	args := []string{
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "error",
		"--servers", fmt.Sprintf("%s:%d", bindAddress, port),
		"--files", followFile,
		"--timeout", fmt.Sprintf("%d", timeoutSeconds),
		"--trustAllHosts",
		"--noColor",
	}
	args = append(args, extraArgs...)

	start := time.Now()
	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t, "", "../dtail", args...)
	if err != nil {
		t.Fatalf("unable to start dtail: %v", err)
	}

	guard := time.NewTimer(guardSeconds * time.Second)
	defer guard.Stop()

	for {
		select {
		case line, ok := <-stdoutCh:
			if ok {
				t.Log("client stdout:", line)
			}
		case line, ok := <-stderrCh:
			if ok {
				t.Log("client stderr:", line)
			}
		case cmdErr := <-cmdErrCh:
			elapsed := time.Since(start)
			t.Logf("dtail exited after %s (err=%v)", elapsed, cmdErr)
			if elapsed > guardSeconds*time.Second {
				t.Fatalf("dtail exited after %s, expected well within %ds", elapsed, guardSeconds)
			}
			return
		case <-guard.C:
			t.Fatalf("dtail did not exit within %ds after --timeout %ds; "+
				"likely reconnecting after the timeout-induced disconnect",
				guardSeconds, timeoutSeconds)
		}
	}
}
