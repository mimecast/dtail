package handlers

// Regression tests for the server-mode output dmap deadlock: with direct output
// enabled, Aggregate.Start used to block until session teardown, so the
// map command never returned, the handler's active-command count never hit
// zero, and the session never shut down — the client hung forever after
// receiving all results. The bug survived because integration-test run mode
// force-disables output (internal/config/initializer.go), so this path was
// never exercised by the integration suite. These tests drive a real
// ServerHandler (real command dispatch, file reads, output aggregate,
// shutdown handshake) exactly like the SSH layer does, just without SSH.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

const testStatsLine = "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|" +
	"MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"

// newMapTestHandler builds a fully wired ServerHandler with direct output
// enabled, exactly as the SSH server would (via NewServerHandler). The user
// is the continuous-query user, which bypasses per-path permission checks so
// the test can read files from t.TempDir().
func newMapTestHandler(t *testing.T) *ServerHandler {
	return newMapTestHandlerWithReaderLogger(t, handlerTestLogger)
}

func newMapTestHandlerWithReaderLogger(t *testing.T, readerLogger logging.Logger) *ServerHandler {
	t.Helper()
	user := &userserver.User{Name: config.ContinuousUser}
	serverCfg := &config.ServerConfig{
		MapreduceLogFormat: "default",
		AuthKeyEnabled:     true,
	}
	handler, err := NewServerHandler(context.Background(), user, Dependencies{
		CatLimiter:   make(chan struct{}, 4),
		TailLimiter:  make(chan struct{}, 4),
		ServerConfig: serverCfg,
		AuthKeyStore: authkey.New(time.Hour, 5),
		Loggers:      HandlerLoggers{Diagnostics: handlerTestLogger, Reader: readerLogger},
	})
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	return handler
}

type readerReadyLogger struct {
	logging.NopLogger
	readyOnce    sync.Once
	longLineOnce sync.Once
	ready        chan struct{}
	longLine     chan struct{}
}

func newReaderReadyLogger() *readerReadyLogger {
	return &readerReadyLogger{
		ready:    make(chan struct{}),
		longLine: make(chan struct{}),
	}
}

func (l *readerReadyLogger) Trace(args ...any) string {
	for _, arg := range args {
		if message, ok := arg.(string); ok && message == "Opened file reader" {
			l.readyOnce.Do(func() { close(l.ready) })
			break
		}
	}
	return ""
}

func (l *readerReadyLogger) Warn(args ...any) string {
	for _, arg := range args {
		if message, ok := arg.(string); ok && message == "Long log line, splitting into multiple lines" {
			l.longLineOnce.Do(func() { close(l.longLine) })
			break
		}
	}
	return fmt.Sprint(args...)
}

func (l *readerReadyLogger) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-l.ready:
	case <-time.After(timeout):
		t.Fatal("tail reader did not open and seek to the initial EOF")
	}
}

func (l *readerReadyLogger) waitForLongLine(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-l.longLine:
	case <-time.After(timeout):
		t.Fatal("tail reader did not consume the long-line processing sentinel")
	}
}

// wrapHandlerCommandsForJoin wraps every registered command handler so the
// test can wait for all asynchronously dispatched command goroutines to run
// to full completion (including their completion callbacks, which log and may
// trigger the session shutdown). Without this join a late dlog call from a
// command goroutine would race with the test-logger restore in cleanup.
func wrapHandlerCommandsForJoin(handler *ServerHandler) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	for name, origHandler := range handler.commands {
		origHandler := origHandler
		handler.commands[name] = func(ctx context.Context, ltx lcontext.LContext,
			argc int, args []string, commandFinished func()) {

			wg.Add(1)
			finished := func() {
				defer wg.Done()
				commandFinished()
			}
			origHandler(ctx, ltx, argc, args, finished)
		}
	}
	return wg
}

// waitForCommandJoin waits until every dispatched command goroutine has fully
// finished; failing the test on timeout instead of leaking goroutines.
func waitForCommandJoin(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for command goroutines to finish")
	}
}

// encodeTestCommand wraps a command in the client wire framing
// (protocol version + base64 + ';' delimiter), mirroring the client-side
// SendMessage implementation.
func encodeTestCommand(command string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	return fmt.Sprintf("protocol %s base64 %s;", protocol.ProtocolCompat, encoded)
}

// testOutput collects everything the handler sends to the "client".
type testOutput struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (o *testOutput) append(p []byte) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(p)
	return o.buf.String()
}

func (o *testOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *testOutput) waitForContains(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	waitForHandlerCondition(t, timeout, "timed out waiting for expected output", func() bool {
		return strings.Contains(o.String(), substr)
	}, func() string {
		return fmt.Sprintf("expected substring %q in %q", substr, o.String())
	})
}

// startTestReader drains handler.Read like the SSH session output
// goroutine (io.Copy) does, collecting all output. When the server initiates
// the close handshake it replies with the client's close acknowledgement, so
// the shutdown sequence completes without waiting for the 5s ack timeout.
//
// writeMu stands in for the single SSH input goroutine of a real session:
// baseHandler.Write is not safe for concurrent use, so the test serializes
// its own command writes and the reader's ack write through this mutex.
func startTestReader(handler *ServerHandler, output *testOutput,
	writeMu *sync.Mutex) <-chan struct{} {

	readerDone := make(chan struct{})
	var ackOnce sync.Once
	go func() {
		defer close(readerDone)
		p := make([]byte, 4096)
		for {
			n, err := handler.Read(p)
			if n > 0 {
				all := output.append(p[:n])
				if strings.Contains(all, ".syn close connection") {
					ackOnce.Do(func() {
						writeMu.Lock()
						defer writeMu.Unlock()
						_, _ = handler.Write([]byte(encodeTestCommand(".ack close connection")))
					})
				}
			}
			if errors.Is(err, io.EOF) {
				return
			}
		}
	}()
	return readerDone
}

func writeTestStatsFile(t *testing.T, lines int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.log")
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		sb.WriteString(testStatsLine)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write stats file: %v", err)
	}
	return path
}

// TestServerModeMapCommandCompletesSession runs a non-interactive dmap
// workload (legacy "map" + "cat" command stream) against a output-enabled
// handler and asserts that the session terminates on its own and delivers
// the aggregated result. Before the fix the session hung forever: the map
// command stayed blocked in Aggregate.Start after all input had been
// read, keeping the active-command count nonzero.
func TestServerModeMapCommandCompletesSession(t *testing.T) {
	handler := newMapTestHandler(t)
	path := writeTestStatsFile(t, 25)

	spec := session.Spec{
		Mode:  omode.MapClient,
		Files: []string{path},
		Query: "from STATS select count($time),$time group by $time",
		Regex: ".",
	}
	commands, commandsErr := spec.Commands()
	if commandsErr != nil {
		t.Fatalf("build commands: %v", commandsErr)
	}

	commandWg := wrapHandlerCommandsForJoin(handler)
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)

	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	writeMu.Lock()
	_, writeErr := handler.Write([]byte(frames.String()))
	writeMu.Unlock()
	if writeErr != nil {
		t.Fatalf("write commands: %v", writeErr)
	}

	select {
	case <-handler.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("session did not shut down after one-shot output map input was exhausted " +
			"(server-mode output dmap deadlock)")
	}

	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	waitForCommandJoin(t, commandWg, 10*time.Second)

	if !strings.Contains(output.String(), "count($time)≔25") {
		t.Fatalf("expected aggregated result count($time)≔25 in output, got: %q", output.String())
	}
}

// TestServerModeMapFollowSessionKeepsStreaming is the negative case for
// over-eager finalization: a continuous map query over a TAILED log never
// reaches input-exhausted, so the output aggregate must keep emitting
// interval results and the session must stay alive. The workload runs via
// SESSION START (the interactive-query bootstrap), matching how continuous
// queries are driven in practice.
func TestServerModeMapFollowSessionKeepsStreaming(t *testing.T) {
	handler := newMapTestHandler(t)
	path := writeTestStatsFile(t, 5)

	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{path},
		Query: "from STATS select count($time),$time group by $time interval 1",
		Regex: ".",
	}
	startCommand, err := spec.StartCommand()
	if err != nil {
		t.Fatalf("build session start command: %v", err)
	}

	commandWg := wrapHandlerCommandsForJoin(handler)
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)

	writeMu.Lock()
	_, writeErr := handler.Write([]byte(encodeTestCommand(startCommand)))
	writeMu.Unlock()
	if writeErr != nil {
		t.Fatalf("write session start: %v", writeErr)
	}
	output.waitForContains(t, sessionAckStartOKPrefix, 5*time.Second)

	// Keep appending lines like a live log file; the tailed input must keep
	// feeding the aggregate across serialization intervals.
	feederStop := make(chan struct{})
	feederDone := make(chan struct{})
	go func() {
		defer close(feederDone)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer func() { _ = file.Close() }()
		for {
			select {
			case <-feederStop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := file.WriteString(testStatsLine + "\n"); err != nil {
					return
				}
			}
		}
	}()

	// An interval-serialized interim aggregate result must arrive while the
	// stream is live (the fix must not finish a follow-mode aggregate).
	output.waitForContains(t, "count($time)≔", 15*time.Second)

	// The session must still be running: tailed input never exhausts.
	select {
	case <-handler.Done():
		t.Fatal("follow-mode output map session shut down prematurely (over-eager finalization)")
	default:
	}

	close(feederStop)
	<-feederDone

	// Tear down like a disconnecting client and join all command goroutines
	// so nothing outlives the test (the session keeps commands alive until
	// their contexts are cancelled by the handler shutdown).
	handler.Shutdown()
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	waitForCommandJoin(t, commandWg, 10*time.Second)

	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("handler did not quiesce: pending=%d active=%d", pending, active)
	}
}

// TestServerlessMapFollowGracefulShutdownDrainsFinalResult exercises the real
// server-side command, tail reader, Processor, aggregate, and protocol
// reader lifecycle used by the in-process connector. The long interval ensures
// the aggregate cannot emit periodically during the test: the result asserted
// below must come from graceful shutdown's final serialization.
func TestServerlessMapFollowGracefulShutdownDrainsFinalResult(t *testing.T) {
	readyLogger := newReaderReadyLogger()
	handler := newMapTestHandlerWithReaderLogger(t, readyLogger)
	handler.readTimings.maxLineLength = len(testStatsLine) + 1
	path := writeTestStatsFile(t, 0)

	spec := session.Spec{
		Mode:    omode.TailClient,
		Files:   []string{path},
		Options: "plain=true:serverless=true",
		Query:   "from STATS select count($time),$time group by $time interval 3600",
		Regex:   "MAPREDUCE:STATS",
	}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}

	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)

	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	writeMu.Lock()
	_, writeErr := handler.Write([]byte(frames.String()))
	writeMu.Unlock()
	if writeErr != nil {
		t.Fatalf("write commands: %v", writeErr)
	}

	waitForHandlerCondition(t, 5*time.Second, "tail command did not become active", func() bool {
		pending, active := handler.PendingAndActive()
		return pending == 1 && active >= 2
	}, func() string {
		pending, active := handler.PendingAndActive()
		return fmt.Sprintf("pending=%d active=%d", pending, active)
	})
	readyLogger.wait(t, 5*time.Second)

	// The oversized, non-matching sentinel follows the three matching records.
	// Its warning proves that the reader consumed everything before it without
	// adding another row to the aggregate.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open stats file for append: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := file.WriteString(testStatsLine + "\n"); err != nil {
			_ = file.Close()
			t.Fatalf("append stats line: %v", err)
		}
	}
	// Tail reads use a 64 KiB buffer. Make the sentinel span more than one read
	// so the long-line path fires before its terminating newline is visible.
	sentinel := strings.Repeat("x", 128*1024) + "\n"
	if _, err := file.WriteString(sentinel); err != nil {
		_ = file.Close()
		t.Fatalf("append processing sentinel: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close stats file: %v", err)
	}
	readyLogger.waitForLongLine(t, 5*time.Second)
	if strings.Contains(output.String(), "count($time)≔") {
		t.Fatalf("aggregate emitted before graceful shutdown: %q", output.String())
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.GracefulShutdownContext(context.Background())
	}()
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown hung with active MapReduce follow input")
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("protocol reader did not observe EOF after graceful shutdown")
	}

	if !strings.Contains(output.String(), "count($time)≔3") {
		t.Fatalf("final aggregate result was not drained before EOF: %q", output.String())
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("handler did not quiesce: pending=%d active=%d", pending, active)
	}
}

// TestServerHandlerShutdownAbortsMapFollowWithoutOutputReader is the network
// teardown counterpart to the graceful serverless test. Once a peer has gone
// away there may be no output consumer, so Shutdown must cancel the follow
// reader and join its processor without attempting final serialization.
func TestServerHandlerShutdownAbortsMapFollowWithoutOutputReader(t *testing.T) {
	readyLogger := newReaderReadyLogger()
	handler := newMapTestHandlerWithReaderLogger(t, readyLogger)
	path := writeTestStatsFile(t, 0)
	spec := session.Spec{
		Mode:  omode.TailClient,
		Files: []string{path},
		Query: "from STATS select count($time),$time group by $time interval 1",
		Regex: ".",
	}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}

	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	if _, writeErr := handler.Write([]byte(frames.String())); writeErr != nil {
		t.Fatalf("write commands: %v", writeErr)
	}

	waitForHandlerCondition(t, 5*time.Second, "tail command did not become active", func() bool {
		pending, active := handler.PendingAndActive()
		return pending == 1 && active >= 2
	}, func() string {
		pending, active := handler.PendingAndActive()
		return fmt.Sprintf("pending=%d active=%d", pending, active)
	})
	readyLogger.wait(t, 5*time.Second)

	// Queue one record after the reader is ready, then wait for interval
	// serialization. This proves the real tail reader and Processor are
	// active while deliberately leaving the protocol queue without a reader.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open stats file for append: %v", err)
	}
	if _, err := file.WriteString(testStatsLine + "\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append stats line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close stats file: %v", err)
	}
	waitForHandlerCondition(t, 5*time.Second, "active tail processor did not queue an aggregate result", func() bool {
		return len(handler.maprMessages) > 0
	}, func() string {
		return fmt.Sprintf("map result queue length=%d", len(handler.maprMessages))
	})
	if got := len(handler.tailLimiter); got != 1 {
		t.Fatalf("tail limiter occupancy before shutdown = %d, want 1", got)
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		handler.Shutdown()
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler Shutdown hung waiting for active MapReduce follow processor")
	}
	if pending, active := handler.PendingAndActive(); pending != 0 || active != 0 {
		t.Fatalf("handler did not quiesce: pending=%d active=%d", pending, active)
	}
	if got := len(handler.tailLimiter); got != 0 {
		t.Fatalf("tail limiter occupancy after shutdown = %d, want 0", got)
	}
	select {
	case <-handler.Done():
	default:
		t.Fatal("handler did not signal transport shutdown")
	}
}
