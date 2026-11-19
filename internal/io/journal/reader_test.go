//go:build linux

package journal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

type captureProcessor struct {
	lines []string
}

func (p *captureProcessor) ProcessLine(lineContent *bytes.Buffer, _ uint64, _ string) error {
	p.lines = append(p.lines, lineContent.String())
	pool.RecycleBytesBuffer(lineContent)
	return nil
}

func (p *captureProcessor) Flush() error {
	return nil
}

func (p *captureProcessor) Close() error {
	return nil
}

type errorProcessor struct {
	err error
}

func (p errorProcessor) ProcessLine(lineContent *bytes.Buffer, _ uint64, _ string) error {
	pool.RecycleBytesBuffer(lineContent)
	return p.err
}

func (p errorProcessor) Flush() error {
	return nil
}

func (p errorProcessor) Close() error {
	return nil
}

type panickingProcessor struct {
	flushes     int
	beforePanic func()
}

func (p *panickingProcessor) ProcessLine(*bytes.Buffer, uint64, string) error {
	if p.beforePanic != nil {
		p.beforePanic()
	}
	panic("processor failed while scanning stdout")
}

func (p *panickingProcessor) Flush() error {
	p.flushes++
	return nil
}

func (*panickingProcessor) Close() error { return nil }

// nonRecyclingErrorProcessor returns an error without recycling the buffer, so a
// test can observe whether processorSink.Emit wrongly recycles a buffer it does
// not own.
type nonRecyclingErrorProcessor struct {
	err error
}

func (p nonRecyclingErrorProcessor) ProcessLine(_ *bytes.Buffer, _ uint64, _ string) error {
	return p.err
}

func (p nonRecyclingErrorProcessor) Flush() error { return nil }

func (p nonRecyclingErrorProcessor) Close() error { return nil }

// TestProcessorSinkEmitDoesNotRecycleOnError is the regression guard for the
// bt0 data race. The line.Processor contract transfers buffer ownership to the
// processor, which recycles it on every return path (the journal-path processors
// DirectLineProcessor and AggregateProcessor recycle unconditionally before
// returning a write error). If
// processorSink.Emit also recycled the buffer on error, the same buffer would be
// returned to the shared pool twice; the pool would then hand one object to two
// Get callers whose concurrent writes race and corrupt data. Emit must therefore
// leave the buffer untouched on error. RecycleBytesBuffer calls buf.Reset(), so
// a stray recycle would clear the payload — assert it survives.
func TestProcessorSinkEmitDoesNotRecycleOnError(t *testing.T) {
	buf := pool.BytesBuffer.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("payload")

	sinkErr := errors.New("processor stopped")
	sink := processorSink{processor: nonRecyclingErrorProcessor{err: sinkErr}}

	err := sink.Emit(context.Background(), buf, 1, 100, "journal-id")
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Emit error = %v, want %v", err, sinkErr)
	}
	if got := buf.String(); got != "payload" {
		t.Fatalf("processorSink.Emit recycled a buffer it does not own (double-recycle regression): buf=%q", got)
	}

	pool.RecycleBytesBuffer(buf)
}

func TestNewReaderFailsWhenJournalctlIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	reader, err := NewReader(nil, "journal", false, nil)
	if err == nil {
		t.Fatal("expected missing journalctl error")
	}
	if reader != nil {
		t.Fatalf("expected nil reader, got %#v", reader)
	}
	if !errors.Is(err, ErrJournalctlNotFound) {
		t.Fatalf("missing journalctl error = %v, want ErrJournalctlNotFound", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing journalctl error = %v, want exec.ErrNotFound", err)
	}
}

func TestStartReadsJournalctlOutputWithoutFollowFlags(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{"alpha", "beta"},
		},
	})

	reader, err := NewReader([]string{"-u", "ssh.service"}, "journal-id", false, make(chan string, 1))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{},
		processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}

	want := []string{"alpha\n", "beta\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected lines: got=%v want=%v", processor.lines, want)
	}

	args := mock.Args(t)
	if strings.Contains(args, "-f") || strings.Contains(args, "-n 0") {
		t.Fatalf("non-follow reader passed follow flags: %q", args)
	}
	if strings.TrimSpace(args) != "-u ssh.service" {
		t.Fatalf("unexpected journalctl args: %q", args)
	}
	if reader.Retry() {
		t.Fatal("non-follow reader should not retry")
	}
}

func TestStartFollowReadsLinesInOrder(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			FollowLines:    []string{"alpha", "beta", "gamma"},
			InterLineDelay: 5 * time.Millisecond,
		},
	})

	reader, err := NewReader([]string{"-u", "ssh.service"}, "journal-id", true, make(chan string, 8))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processor := &flushCountingProcessor{}
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop())
	}()

	// Poll until all three follow lines have arrived (the reader appends from its
	// goroutine while following).
	deadline := time.After(2 * time.Second)
	var got []string
	for {
		got = processor.snapshot()
		if len(got) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("follow reader did not deliver 3 lines: got=%v", got)
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("follow reader returned error after cancel: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("follow reader did not stop promptly after cancellation")
	}

	want := []string{"alpha\n", "beta\n", "gamma\n"}
	if !reflect.DeepEqual(got[:3], want) {
		t.Fatalf("unexpected follow lines: got=%v want=%v", got, want)
	}
}

func TestStartFollowPassesFollowFlagsAndTerminatesOnCancel(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{"ready"},
		},
	})

	reader, err := NewReader([]string{"-u", "ssh.service"}, "journal-id", true, make(chan string, 1))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	processor := &flushCountingProcessor{}
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop())
	}()

	// Wait for the first follow line to arrive before probing the process.
	deadline := time.After(2 * time.Second)
	for {
		got := processor.snapshot()
		if len(got) >= 1 {
			if got[0] != "ready\n" {
				t.Fatalf("unexpected first line: %q", got[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("follow reader did not deliver the first line")
		case <-time.After(2 * time.Millisecond):
		}
	}

	pid := mockPID(t, mock)
	if !processExists(pid) {
		t.Fatalf("fake journalctl process %d does not exist before cancel", pid)
	}

	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow reader returned error after cancel: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("follow reader did not stop promptly after cancellation")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("follow reader returned too slowly after cancel: %s", elapsed)
	}

	args := mock.Args(t)
	if !strings.Contains(args, "-f -n 0") {
		t.Fatalf("follow reader did not pass follow flags: %q", args)
	}
	if !mock.Terminated(t) {
		t.Fatal("journalctl did not observe SIGTERM")
	}
	if processExists(pid) {
		t.Fatalf("fake journalctl process %d still exists after reader returned", pid)
	}
	if !reader.Retry() {
		t.Fatal("follow reader should retry")
	}
}

func TestStartSurfacesStderrAsServerMessages(t *testing.T) {
	const stderrLineCount = 128
	stderrLines := makeJournalStderrLines(stderrLineCount)

	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:  []string{"alpha"},
			Stderr: stderrLines,
		},
	})

	serverMessages := make(chan string, stderrLineCount)
	reader, err := NewReader(nil, "journal-id", false, serverMessages)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	if err := reader.Start(context.Background(), lcontext.LContext{},
		&captureProcessor{}, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}

	for i, stderrLine := range stderrLines {
		select {
		case message := <-serverMessages:
			want := "journalctl stderr: " + stderrLine + "\n"
			if message != want {
				t.Fatalf("server message %d = %q, want %q", i, message, want)
			}
		default:
			t.Fatalf("missing stderr server message %d of %d", i+1, len(stderrLines))
		}
	}
}

func TestStartBoundsInheritedStderrAndPreservesBufferedMessages(t *testing.T) {
	const stderrLineCount = 16
	stderrLines := makeJournalStderrLines(stderrLineCount)

	tempDir := t.TempDir()
	pidPath := filepath.Join(tempDir, "descendant.pid")
	scriptPath := filepath.Join(tempDir, "journalctl")
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("printf 'alpha\\n'\n")
	for _, stderrLine := range stderrLines {
		script.WriteString("printf '%s\\n' '")
		script.WriteString(stderrLine)
		script.WriteString("' >&2\n")
	}
	script.WriteString("sleep 30 >/dev/null &\n")
	script.WriteString("printf '%s\\n' \"$!\" > \"$DTAIL_JOURNAL_DESCENDANT_PID\"\n")
	if err := os.WriteFile(scriptPath, []byte(script.String()), 0o700); err != nil {
		t.Fatalf("write journalctl test script: %v", err)
	}
	t.Setenv("DTAIL_JOURNAL_DESCENDANT_PID", pidPath)
	t.Cleanup(func() {
		pidBytes, err := os.ReadFile(pidPath)
		if err != nil {
			if !os.IsNotExist(err) {
				t.Errorf("read descendant pid: %v", err)
			}
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil {
			t.Errorf("parse descendant pid: %v", err)
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	serverMessages := make(chan string, stderrLineCount)
	reader := &Reader{
		journalctlPath: scriptPath,
		sourceID:       "journal-id",
		serverMessages: serverMessages,
	}
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(context.Background(), lcontext.LContext{},
			&captureProcessor{}, regex.NewNoop())
	}()

	select {
	case err := <-done:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("reader error = %v, want exec.ErrWaitDelay", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung on inherited journalctl stderr")
	}

	for i, stderrLine := range stderrLines {
		select {
		case message := <-serverMessages:
			want := "journalctl stderr: " + stderrLine + "\n"
			if message != want {
				t.Fatalf("server message %d = %q, want %q", i, message, want)
			}
		default:
			t.Fatalf("missing stderr server message %d of %d", i+1, len(stderrLines))
		}
	}
}

func TestStartPropagatesStderrForwarderPanic(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{HoldOpen: true},
	})
	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	reader.forwardStderrHook = func(context.Context, io.Reader) error {
		close(hookStarted)
		<-releaseHook
		panic("stderr child failed")
	}

	done := make(chan error, 1)
	go func() {
		done <- reader.Start(context.Background(), lcontext.LContext{},
			&captureProcessor{}, regex.NewNoop())
	}()
	select {
	case <-hookStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stderr forwarder did not start")
	}
	pid := waitForMockPID(t, mock)
	close(releaseHook)
	select {
	case err := <-done:
		if !errors.Is(err, fs.ErrReaderWorkerPanic) || !strings.Contains(err.Error(), "stderr child failed") {
			t.Fatalf("reader error = %v, want propagated stderr child panic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop after stderr child panic")
	}
	if processExists(pid) {
		t.Fatalf("journalctl process %d survived stderr child panic", pid)
	}
}

func TestRunPrioritizesStderrWorkerPanicOverParentCancellation(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{HoldOpen: true},
	})
	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	stderrStarted := make(chan struct{})
	releaseStderr := make(chan struct{})
	reader.forwardStderrHook = func(context.Context, io.Reader) error {
		close(stderrStarted)
		<-releaseStderr
		panic("stderr child failed after cancellation")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{},
			&captureProcessor{}, regex.NewNoop())
	}()

	select {
	case <-stderrStarted:
	case <-time.After(time.Second):
		t.Fatal("stderr worker did not start")
	}
	cancel()
	close(releaseStderr)

	select {
	case err := <-done:
		if !errors.Is(err, fs.ErrReaderWorkerPanic) ||
			!strings.Contains(err.Error(), "stderr child failed after cancellation") {
			t.Fatalf("reader error = %v, want stderr worker panic despite cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop after cancellation and stderr panic")
	}
}

func TestRunJoinsProcessorFailureWithWaitWorkerPanic(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{Lines: []string{"line"}},
	})
	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	reader.waitCommandHook = func(cmd *exec.Cmd) error {
		_ = cmd.Wait()
		panic("wait child failed after scan")
	}

	processorErr := errors.New("processor failed")
	err = reader.Start(context.Background(), lcontext.LContext{},
		errorProcessor{err: processorErr}, regex.NewNoop())
	if !errors.Is(err, processorErr) {
		t.Fatalf("reader error = %v, want processor failure in joined chain", err)
	}
	if !errors.Is(err, fs.ErrReaderWorkerPanic) || !strings.Contains(err.Error(), "wait child failed after scan") {
		t.Fatalf("reader error = %v, want wait worker panic in joined chain", err)
	}
}

func TestRunKillsAndReapsTermIgnoringChildWhenWaitPanicsBeforeWait(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:         []string{"line"},
			HoldOpen:      true,
			IgnoreSIGTERM: true,
		},
	})
	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	reader.waitCommandHook = func(*exec.Cmd) error {
		panic("wait child failed before reaping")
	}

	processorErr := errors.New("processor failed")
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(context.Background(), lcontext.LContext{},
			errorProcessor{err: processorErr}, regex.NewNoop())
	}()

	select {
	case err := <-done:
		if !errors.Is(err, processorErr) {
			t.Fatalf("reader error = %v, want processor failure in joined chain", err)
		}
		if !errors.Is(err, fs.ErrReaderWorkerPanic) ||
			!strings.Contains(err.Error(), "wait child failed before reaping") {
			t.Fatalf("reader error = %v, want fatal wait worker panic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung after pre-Wait panic with SIGTERM-ignoring child")
	}

	pid := mockPID(t, mock)
	if processExists(pid) {
		t.Fatalf("journalctl process %d survived or remained unreaped after wait panic", pid)
	}
}

func TestRunReapsAfterWaitPanicWhileStderrDeliveryBlocked(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:  []string{"line"},
			Stderr: makeJournalStderrLines(128),
		},
	})
	// The unread channel blocks delivery of the first diagnostic. Draining the
	// process pipe must continue independently so stdout can close and the
	// recovered Wait panic can still reap the child.
	reader, err := NewReader(nil, "journal-id", false, make(chan string))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	reader.waitCommandHook = func(*exec.Cmd) error {
		panic("wait child failed with blocked stderr delivery")
	}

	done := make(chan error, 1)
	go func() {
		done <- reader.Start(context.Background(), lcontext.LContext{},
			&captureProcessor{}, regex.NewNoop())
	}()

	select {
	case err := <-done:
		if !errors.Is(err, fs.ErrReaderWorkerPanic) ||
			!strings.Contains(err.Error(), "wait child failed with blocked stderr delivery") {
			t.Fatalf("reader error = %v, want fatal wait worker panic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung after Wait panic with blocked stderr delivery")
	}

	pid := mockPID(t, mock)
	if processExists(pid) {
		t.Fatalf("journalctl process %d survived or remained unreaped after wait panic", pid)
	}
}

func TestRunRecoversScanProcessorPanicAndReapsTermIgnoringChild(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:         []string{"line"},
			HoldOpen:      true,
			IgnoreSIGTERM: true,
		},
	})
	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	processor := &panickingProcessor{beforePanic: cancel}

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{},
			processor, regex.NewNoop())
	}()

	select {
	case err := <-done:
		if !errors.Is(err, fs.ErrReaderWorkerPanic) ||
			!strings.Contains(err.Error(), "stdout scanning") ||
			!strings.Contains(err.Error(), "processor failed while scanning stdout") {
			t.Fatalf("reader error = %v, want fatal stdout processor panic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung after processor panic with SIGTERM-ignoring child")
	}

	if elapsed := time.Since(started); elapsed < processTerminateGrace {
		t.Fatalf("reader returned before SIGTERM grace and kill/reap: %s", elapsed)
	}
	if processor.flushes != 1 {
		t.Fatalf("terminal processor flushes = %d, want 1 after recovered scan panic", processor.flushes)
	}
	pid := mockPID(t, mock)
	if processExists(pid) {
		t.Fatalf("journalctl process %d survived or remained unreaped after processor panic", pid)
	}
}

func TestWaitForJournalctlRecoversWaitChildPanic(t *testing.T) {
	err := waitForJournalctlWith(&exec.Cmd{}, func() error {
		panic("wait child failed")
	})
	if !errors.Is(err, fs.ErrReaderWorkerPanic) || !strings.Contains(err.Error(), "wait child failed") {
		t.Fatalf("wait error = %v, want propagated child panic", err)
	}
}

func TestStartReturnsExitErrorAndForwardsStderrOnNonZeroExit(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:    []string{"before failure"},
			Stderr:   []string{"boom"},
			ExitCode: 17,
		},
	})

	serverMessages := make(chan string, 2)
	reader, err := NewReader(nil, "journal-id", false, serverMessages)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &captureProcessor{}
	err = reader.Start(context.Background(), lcontext.LContext{},
		processor, regex.NewNoop())
	if err == nil {
		t.Fatal("expected non-zero journalctl exit error")
	}
	if !strings.Contains(err.Error(), "journalctl failed") {
		t.Fatalf("unexpected non-zero exit error: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want exec.ExitError", err)
	}
	if got := exitErr.ExitCode(); got != 17 {
		t.Fatalf("exit code = %d, want 17", got)
	}
	if reader.Retry() {
		t.Fatal("non-follow reader should not retry after non-zero exit")
	}

	if want := []string{"before failure\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected lines before failure: got=%v want=%v", processor.lines, want)
	}

	select {
	case message := <-serverMessages:
		if message != "journalctl stderr: boom\n" {
			t.Fatalf("unexpected server message: %q", message)
		}
	default:
		t.Fatal("expected stderr server message")
	}
}

func TestStartReadsLongJournalLine(t *testing.T) {
	const longLineLength = 70 * 1024

	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			LongLineLength: longLineLength,
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{},
		processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}

	want := []string{strings.Repeat("x", longLineLength) + "\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		gotLen := 0
		if len(processor.lines) > 0 {
			gotLen = len(processor.lines[0])
		}
		t.Fatalf("unexpected long line: got len=%d want len=%d", gotLen, len(want[0]))
	}
}

func TestStartDropsPartialLineAtShutdown(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			PartialLine: "unterminated",
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{},
		processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}
	if len(processor.lines) != 0 {
		t.Fatalf("partial line without trailing newline was emitted: %v", processor.lines)
	}
}

func TestStartPreservesUTF8Lines(t *testing.T) {
	want := []string{"żółć 🚀\n", "東京 café\n"}
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{strings.TrimSuffix(want[0], "\n"), strings.TrimSuffix(want[1], "\n")},
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &captureProcessor{}
	if err := reader.Start(context.Background(), lcontext.LContext{},
		processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}

	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected UTF-8 lines: got=%v want=%v", processor.lines, want)
	}
}

func TestConcurrentReadersDifferentUnitsDoNotInterfere(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Units: map[string]journaltest.Invocation{
			"alpha.service": {
				Lines: []string{"alpha-1", "alpha-2"},
			},
			"beta.service": {
				Lines: []string{"beta-1", "beta-2"},
			},
		},
	})

	type result struct {
		lines []string
		err   error
	}

	runReader := func(unit string) <-chan result {
		resultCh := make(chan result, 1)
		go func() {
			reader, err := NewReader([]string{"-u", unit}, unit, false, nil)
			if err != nil {
				resultCh <- result{err: err}
				return
			}
			processor := &captureProcessor{}
			err = reader.Start(context.Background(), lcontext.LContext{},
				processor, regex.NewNoop())
			resultCh <- result{lines: processor.lines, err: err}
		}()
		return resultCh
	}

	alphaCh := runReader("alpha.service")
	betaCh := runReader("beta.service")

	alpha := <-alphaCh
	beta := <-betaCh

	if alpha.err != nil {
		t.Fatalf("alpha reader: %v", alpha.err)
	}
	if beta.err != nil {
		t.Fatalf("beta reader: %v", beta.err)
	}
	if want := []string{"alpha-1\n", "alpha-2\n"}; !reflect.DeepEqual(alpha.lines, want) {
		t.Fatalf("alpha lines: got=%v want=%v", alpha.lines, want)
	}
	if want := []string{"beta-1\n", "beta-2\n"}; !reflect.DeepEqual(beta.lines, want) {
		t.Fatalf("beta lines: got=%v want=%v", beta.lines, want)
	}
}

func TestStartAppliesRegexAndLocalContext(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{"before", "match", "context", "skip"},
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	re, err := regex.New("match", regex.Default)
	if err != nil {
		t.Fatalf("new regex: %v", err)
	}
	processor := &captureProcessor{}

	err = reader.Start(
		context.Background(),
		lcontext.LContext{BeforeContext: 1, AfterContext: 1, MaxCount: 1},
		processor,
		re,
	)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("start reader: %v", err)
	}

	want := []string{"before\n", "match\n", "context\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, want)
	}
}

// flushCountingProcessor records processed lines and how many times Flush was
// invoked, guarded by a mutex because the reader drives it from a goroutine.
type flushCountingProcessor struct {
	mu         sync.Mutex
	lines      []string
	flushCount int
}

func (p *flushCountingProcessor) ProcessLine(lineContent *bytes.Buffer, _ uint64, _ string) error {
	p.mu.Lock()
	p.lines = append(p.lines, lineContent.String())
	p.mu.Unlock()
	pool.RecycleBytesBuffer(lineContent)
	return nil
}

func (p *flushCountingProcessor) Flush() error {
	p.mu.Lock()
	p.flushCount++
	p.mu.Unlock()
	return nil
}

func (p *flushCountingProcessor) Close() error { return nil }

func (p *flushCountingProcessor) counts() (lines, flushes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.lines), p.flushCount
}

// snapshot returns a copy of the processed lines so a test can inspect ordered
// content while the follow reader keeps appending from its goroutine.
func (p *flushCountingProcessor) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

// TestStartFollowFlushesEachLine is the regression guard for the
// output batching fix (task 0t0). A follow read blocks in r.run until journalctl
// is stopped, so a batching output writer would hold live lines in its 64KB
// buffer and the client would see nothing until the stream ends. The reader
// must therefore flush the processor after every line while following. If the
// per-line flush is dropped, flushCount stays at zero during the follow and
// this test fails.
func TestStartFollowFlushesEachLine(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			FollowLines:    []string{"one", "two", "three"},
			InterLineDelay: 5 * time.Millisecond,
		},
	})

	reader, err := NewReader([]string{"-u", "ssh.service"}, "journal-id", true, make(chan string, 8))
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processor := &flushCountingProcessor{}
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop())
	}()

	// While the follow is still live, each of the three lines must trigger a
	// flush. Poll until at least three flushes are observed.
	deadline := time.After(2 * time.Second)
	for {
		lines, flushes := processor.counts()
		if lines >= 3 && flushes >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("follow reader did not flush per line: lines=%d flushes=%d", lines, flushes)
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("follow reader returned error after cancel: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("follow reader did not stop promptly after cancellation")
	}
}

// TestStartNonFollowFlushesOnceAtEnd pins the complementary
// behavior: a non-follow snapshot read keeps the batching benefit and only
// flushes once, at the end, rather than per line.
func TestStartNonFollowFlushesOnceAtEnd(t *testing.T) {
	journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines: []string{"a", "b", "c", "d"},
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processor := &flushCountingProcessor{}
	if err := reader.Start(context.Background(),
		lcontext.LContext{}, processor, regex.NewNoop()); err != nil {
		t.Fatalf("start reader: %v", err)
	}

	lines, flushes := processor.counts()
	if lines != 4 {
		t.Fatalf("expected 4 processed lines, got %d", lines)
	}
	// Only the single terminal flush in runWithProcessor should fire.
	if flushes != 1 {
		t.Fatalf("expected exactly 1 flush for a non-follow read, got %d", flushes)
	}
}

func TestStartErrorTerminatesJournalctl(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:    []string{"ready"},
			HoldOpen: true,
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processorErr := errors.New("processor stopped")
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(
			context.Background(),
			lcontext.LContext{},
			errorProcessor{err: processorErr},
			regex.NewNoop(),
		)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, processorErr) {
			t.Fatalf("unexpected reader error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung after processor error")
	}

	if !mock.Terminated(t) {
		t.Fatal("journalctl did not observe SIGTERM")
	}
}

func TestStartErrorKillsTermIgnoringJournalctl(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{
		Default: journaltest.Invocation{
			Lines:         []string{"ready"},
			HoldOpen:      true,
			IgnoreSIGTERM: true,
		},
	})

	reader, err := NewReader(nil, "journal-id", false, nil)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	processorErr := errors.New("processor stopped")
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- reader.Start(
			context.Background(),
			lcontext.LContext{},
			errorProcessor{err: processorErr},
			regex.NewNoop(),
		)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, processorErr) {
			t.Fatalf("unexpected reader error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader hung after TERM-ignoring journalctl")
	}

	if elapsed := time.Since(started); elapsed < processTerminateGrace {
		t.Fatalf("reader returned before kill grace elapsed: %s", elapsed)
	}
	if !mock.Terminated(t) {
		t.Fatal("journalctl did not observe SIGTERM before kill")
	}
	pid := mockPID(t, mock)
	if processExists(pid) {
		t.Fatalf("fake journalctl process %d still exists after reader returned", pid)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func mockPID(t *testing.T, mock *journaltest.Mock) int {
	t.Helper()

	pid, err := strconv.Atoi(strings.TrimSpace(readFileString(t, mock.PIDFile)))
	if err != nil {
		t.Fatalf("parse fake journalctl pid: %v", err)
	}
	return pid
}

func waitForMockPID(t *testing.T, mock *journaltest.Mock) int {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		content, err := os.ReadFile(mock.PIDFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
			if parseErr == nil {
				return pid
			}
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for fake journalctl pid in %s", mock.PIDFile)
			return 0
		}
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func makeJournalStderrLines(count int) []string {
	payload := strings.Repeat("x", 1024)
	lines := make([]string, count)
	for i := range lines {
		lines[i] = "journal warning " + strconv.Itoa(i) + " " + payload
	}
	return lines
}
