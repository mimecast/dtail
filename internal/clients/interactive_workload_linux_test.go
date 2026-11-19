//go:build linux

package clients

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

// switchE2EQuery is the query of TestInteractiveDTailSwitchesWorkloadEndToEnd;
// its interval of 1 s makes the result show up within the test.
const switchE2EQuery = "from STATS select count($line) group by $hostname interval 1"

// TestInteractiveDTailSwitchesWorkloadEndToEnd runs the client of an
// interactive dtail (NewInteractiveTailClient) in serverless mode with a real
// pseudo terminal as its control TTY and a real file, and switches its
// running session from following lines to a query and back
// (switchToQuery, switchToTail), as the control commands will: the lines
// matching the tail regex are printed, then the query's result, then the
// lines matching the new regex. "framed" routes the in-process server's
// payload through the client handler (a MaprHandler) as from a remote
// dserver, "direct" lets it write its payload itself as serverless mode
// does.
func TestInteractiveDTailSwitchesWorkloadEndToEnd(t *testing.T) {
	for _, framed := range []bool{false, true} {
		name := "direct"
		if framed {
			name = "framed"
		}
		t.Run(name, func(t *testing.T) {
			runSwitchE2E(t, framed)
		})
	}
}

func runSwitchE2E(t *testing.T, framed bool) {
	master, ttyPath := openTestPTY(t)
	file := filepath.Join(t.TempDir(), "switch.log")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, logger, direct := newSwitchE2EClient(t, file, ttyPath, framed)
	payload := func() string {
		if framed {
			return strings.Join(logger.writtenLines(), "")
		}
		return direct.String()
	}
	stopFeeder := feedSwitchE2ELines(t, file)
	defer stopFeeder()

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Interactive query control enabled", done)
	waitForSwitchE2E(t, "PLAIN line", payload)
	// A --plain line that starts like an aggregate frame is payload while
	// the session follows lines.
	waitForSwitchE2E(t, "AGGREGATE|host|x\n", payload)

	retrySwitch(t, func() error { _, err := c.switchToQuery(switchE2EQuery); return err })
	results := func() string { return strings.Join(logger.writtenLines(), "") }
	waitForSwitchE2E(t, "count($line)", results)

	if _, err := c.switchToTail("WARN"); err != nil {
		t.Fatalf("switchToTail(WARN): %v", err)
	}
	waitForSwitchE2E(t, "WARN line", payload)

	if _, err := master.WriteString(":quit\n"); err != nil {
		t.Fatalf("write :quit: %v", err)
	}
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("Start status = %d, want 0", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive dtail did not end")
	}
}

// newSwitchE2EClient returns the client of runSwitchE2E following the PLAIN
// lines of file, its client logger and the direct serverless output.
func newSwitchE2EClient(t *testing.T, file, ttyPath string, framed bool) (*MaprClient,
	*pausingTestLogger, *lockedBuffer) {
	t.Helper()
	logger := newPausingTestLogger()
	args := config.Args{
		What: file, RegexStr: "PLAIN|^AGGREGATE", Serverless: true, Plain: true, SSHArgs: config.SSHArgs{UserName: "switch-user"},
		InteractiveQuery: true, ControlTTYPath: ttyPath, ConnectionsPerCPU: 1,
	}
	c, err := NewInteractiveTailClient(args, clientTestRuntimeConfig(),
		NewLoggerDependencies(logger, logging.NopLogger{}, logging.NopLogger{}))
	if err != nil {
		t.Fatalf("NewInteractiveTailClient: %v", err)
	}
	direct := &lockedBuffer{}
	c.runtime.stdout = func() io.Writer { return direct }
	if framed {
		_, spec := c.snapshotConnectionState()
		commands, err := spec.Commands()
		if err != nil {
			t.Fatal(err)
		}
		c.connections[0] = connectors.NewServerless(args.UserName, c.profile.newHandler("local(serverless)"),
			commands, spec, true, framedServerlessFactory{runtime: c.runtime}, logging.NopLogger{})
	}
	return c, logger, direct
}

// feedSwitchE2ELines appends a PLAIN line, a line starting like an aggregate
// frame (AGGREGATE|), a WARN and a STATS line to file every 50 ms until the
// returned stop is called, so every workload finds lines.
func feedSwitchE2ELines(t *testing.T, file string) (stop func()) {
	t.Helper()
	out, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	quit, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		defer func() { _ = out.Close() }()
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			case <-time.After(50 * time.Millisecond):
			}
			_, _ = fmt.Fprintf(out, "PLAIN line %d\nAGGREGATE|host|x\nWARN line %d\nINFO|1002-071143|1|stats.go:56|8|13|7|"+
				"0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=%d|lifetimeConnections=%d\n", i, i, i%3, i)
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit); <-stopped }) }
}

// retrySwitch runs switchF until it succeeds: the first switch may come
// before the session started (no server has an active session yet).
func retrySwitch(t *testing.T, switchF func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := switchF()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("switch: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForSwitchE2E(t *testing.T, want string, output func() string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(output(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q within 15s in %q", want, output())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
