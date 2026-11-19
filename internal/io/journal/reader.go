//go:build linux

// Package journal provides a journalctl-backed file reader.
package journal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

const (
	defaultSourceID       = "journal"
	journalctlCommand     = "journalctl"
	maxScannerTokenSize   = 1024 * 1024
	processTerminateGrace = 200 * time.Millisecond
)

var errStopReading = errors.New("stop journal reading")

// ErrJournalctlNotFound reports that journalctl could not be found on PATH.
var ErrJournalctlNotFound = errors.New("journalctl not found")

// Reader reads journal entries by executing journalctl.
type Reader struct {
	journalctlPath    string
	args              []string
	sourceID          string
	serverMessages    chan<- string
	follow            bool
	forwardStderrHook func(context.Context, io.Reader) error
	waitCommandHook   func(*exec.Cmd) error
}

// journalStderrQueue decouples draining the process pipe from delivery to the
// session's bounded message channel. That distinction matters during Wait:
// os/exec cannot interrupt its copy goroutine while the goroutine is blocked
// writing to a caller-owned io.PipeWriter.
type journalStderrQueue struct {
	mu       sync.Mutex
	messages []string
	closed   bool
	wake     chan struct{}
}

func newJournalStderrQueue() *journalStderrQueue {
	return &journalStderrQueue{wake: make(chan struct{}, 1)}
}

func (q *journalStderrQueue) push(message string) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.messages = append(q.messages, message)
	q.mu.Unlock()
	q.signal()
}

func (q *journalStderrQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *journalStderrQueue) next(ctx context.Context) (string, bool) {
	for {
		select {
		case <-ctx.Done():
			return "", false
		default:
		}

		q.mu.Lock()
		if len(q.messages) > 0 {
			message := q.messages[0]
			q.messages[0] = ""
			q.messages = q.messages[1:]
			if len(q.messages) == 0 {
				q.messages = nil
			}
			q.mu.Unlock()
			return message, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return "", false
		}

		select {
		case <-ctx.Done():
			return "", false
		case <-q.wake:
		}
	}
}

func (q *journalStderrQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

var _ fs.FileReader = (*Reader)(nil)

// NewReader returns a journalctl-backed file reader.
func NewReader(args []string, sourceID string, follow bool, serverMessages chan<- string) (*Reader, error) {
	journalctlPath, err := exec.LookPath(journalctlCommand)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournalctlNotFound, err)
	}
	if sourceID == "" {
		sourceID = defaultSourceID
	}

	copiedArgs := append([]string(nil), args...)
	return &Reader{
		journalctlPath: journalctlPath,
		args:           copiedArgs,
		sourceID:       sourceID,
		serverMessages: serverMessages,
		follow:         follow,
	}, nil
}

// Start reads journalctl stdout and sends matching lines to processor.
func (r *Reader) Start(ctx context.Context, ltx lcontext.LContext,
	processor line.Processor, re regex.Regex) error {

	return r.runWithProcessor(ctx, ltx, processor, re)
}

// FilePath returns a stable journalctl command description.
func (r *Reader) FilePath() string {
	if len(r.args) == 0 {
		return journalctlCommand
	}
	return journalctlCommand + " " + strings.Join(r.args, " ")
}

// Retry reports whether journalctl should be restarted after it exits.
func (r *Reader) Retry() bool {
	return r.follow
}

func (r *Reader) runWithProcessor(ctx context.Context, ltx lcontext.LContext,
	processor line.Processor, re regex.Regex) error {

	sink := processorSink{processor: processor}

	// In follow mode r.run blocks until journalctl is stopped, so a batching
	// processor (the NetworkWriter, which buffers up to 64KB before
	// sending) would hold live lines in its buffer and the client would never
	// see interactive output. Flush after every scanned line while following so
	// journal follow output reaches the client promptly — the same latency
	// guarantee the file follow path gets from its per-read-chunk flush. A
	// non-follow snapshot read keeps the batching benefit and flushes once at
	// the end below.
	var flushLine func() error
	if r.follow {
		flushLine = processor.Flush
	}

	err := r.run(ctx, ltx, sink, re, flushLine)
	if flushErr := processor.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}
	return err
}

func (r *Reader) run(ctx context.Context, ltx lcontext.LContext, sink journalSink,
	re regex.Regex, flushLine func() error) error {

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := r.command(runCtx)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open journalctl stdout: %w", err)
	}
	stderr, stderrWriter := io.Pipe()
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return fmt.Errorf("start journalctl: %w", err)
	}

	stderrQueue := newJournalStderrQueue()
	stderrDrainDone := make(chan error, 1)
	forwardStderr := func(ctx context.Context, stderr io.Reader) error {
		return scanJournalStderr(ctx, stderr, stderrQueue.push)
	}
	if r.forwardStderrHook != nil {
		forwardStderr = r.forwardStderrHook
	}
	go func() {
		defer func() { _ = stderr.Close() }()
		stderrErr := runJournalChild("stderr forwarding", func() error {
			return forwardStderr(runCtx, stderr)
		})
		stderrQueue.close()
		if stderrErr != nil {
			cancel()
		}
		stderrDrainDone <- stderrErr
	}()

	stderrDeliveryDone := make(chan error, 1)
	go func() {
		stderrErr := runJournalChild("stderr delivery", func() error {
			return r.deliverJournalStderr(runCtx, stderrQueue)
		})
		if stderrErr != nil {
			cancel()
		}
		stderrDeliveryDone <- stderrErr
	}()

	filter := newJournalFilter(ltx, sink, re, r.sourceID)
	scanErr := runJournalChild("stdout scanning", func() error {
		return r.scanStdout(runCtx, stdout, filter, flushLine)
	})
	if scanErr != nil {
		// Stop the child before waiting; WaitDelay kills it if SIGTERM does not
		// make it exit and bounds inherited stderr descriptors.
		cancel()
	}
	waitCommand := cmd.Wait
	if r.waitCommandHook != nil {
		waitCommand = func() error { return r.waitCommandHook(cmd) }
	}
	waitErr := waitForJournalctlWith(cmd, waitCommand)
	if errors.Is(waitErr, fs.ErrReaderWorkerPanic) {
		// A recovered Wait panic is fatal. Cancel before joining the stderr
		// forwarder so a full server-message channel cannot strand it.
		cancel()
	}
	// cmd.Stderr is an io.Writer, so Wait owns and joins the child-side copy.
	// Closing our writer after that copy finishes publishes EOF to the line
	// forwarder. Unlike StderrPipe, WaitDelay can close the os/exec-owned pipe
	// if a descendant inherits stderr and keeps it open.
	_ = stderrWriter.Close()
	stderrDrainErr := <-stderrDrainDone
	stderrDeliveryErr := <-stderrDeliveryDone
	stderrErr := errors.Join(stderrDrainErr, stderrDeliveryErr)
	filter.Close()

	// A recovered child panic is a fatal reader failure even when it races
	// with parent cancellation or a scanner/processor failure. Inspect both
	// child results only after they have completed, and keep the scan error in
	// the chain when it provides additional context.
	workerPanicErr := errors.Join(
		journalWorkerPanic(scanErr),
		journalWorkerPanic(stderrErr),
		journalWorkerPanic(waitErr),
	)
	if workerPanicErr != nil {
		if errors.Is(scanErr, fs.ErrReaderWorkerPanic) {
			scanErr = nil
		}
		return errors.Join(scanErr, workerPanicErr)
	}

	if ctx.Err() != nil || errors.Is(scanErr, errStopReading) {
		return nil
	}
	if stderrErr != nil {
		return stderrErr
	}
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("journalctl failed: %w", waitErr)
	}
	return nil
}

func journalWorkerPanic(err error) error {
	if errors.Is(err, fs.ErrReaderWorkerPanic) {
		return err
	}
	return nil
}

func (r *Reader) command(ctx context.Context) *exec.Cmd {
	args := r.commandArgs()
	cmd := exec.CommandContext(ctx, r.journalctlPath, args...)
	cmd.Cancel = func() error {
		terminateProcess(cmd.Process)
		return nil
	}
	cmd.WaitDelay = processTerminateGrace
	return cmd
}

func (r *Reader) commandArgs() []string {
	args := append([]string(nil), r.args...)
	if r.follow {
		args = append(args, "-f", "-n", "0")
	}
	return args
}

func terminateProcess(process *os.Process) {
	if process == nil {
		return
	}
	_ = process.Signal(syscall.SIGTERM)
}

func killProcess(process *os.Process) {
	if process == nil {
		return
	}
	_ = process.Kill()
}

func waitForJournalctlWith(cmd *exec.Cmd, wait func() error) error {
	return cleanupJournalWaitPanic(cmd, runJournalChild("journalctl wait", wait))
}

func cleanupJournalWaitPanic(cmd *exec.Cmd, waitErr error) error {
	if !errors.Is(waitErr, fs.ErrReaderWorkerPanic) {
		return waitErr
	}

	// The injected/underlying wait callback may have panicked before calling
	// cmd.Wait. Kill the process and synchronously reap it so the child cannot
	// survive or remain as a zombie.
	killProcess(cmd.Process)
	reapErr := runJournalChild("journalctl reap after wait panic", cmd.Wait)
	if errors.Is(reapErr, fs.ErrReaderWorkerPanic) {
		return errors.Join(waitErr, reapErr)
	}
	return waitErr
}

func runJournalChild(scope string, child func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: journal %s: %v\n%s", fs.ErrReaderWorkerPanic, scope, recovered, debug.Stack())
		}
	}()
	return child()
}

func (r *Reader) scanStdout(ctx context.Context, stdout io.Reader, filter *journalFilter,
	flushLine func() error) error {

	scanner := bufio.NewScanner(stdout)
	bufPtr := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(bufPtr)

	scanner.Buffer(*bufPtr, maxScannerTokenSize)
	scanner.Split(scanLinesPreserveEndings)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		lineBuf := pool.BytesBuffer.Get().(*bytes.Buffer)
		lineBuf.Write(scanner.Bytes())
		if err := filter.Process(ctx, lineBuf); err != nil {
			return err
		}

		// In follow mode, push any buffered line straight to the client so
		// batching never delays live output. flushLine is nil for non-follow
		// reads (they batch and flush once at the end) and for the immediate
		// channel sink.
		if flushLine != nil {
			if err := flushLine(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan journalctl stdout: %w", err)
	}
	return nil
}

func scanJournalStderr(ctx context.Context, stderr io.Reader, emit func(string)) error {
	scanner := bufio.NewScanner(stderr)
	bufPtr := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(bufPtr)

	scanner.Buffer(*bufPtr, maxScannerTokenSize)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		emit(fmt.Sprintf("journalctl stderr: %s\n", scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil
		}
		return fmt.Errorf("scan journalctl stderr: %w", err)
	}
	return nil
}

func (r *Reader) deliverJournalStderr(ctx context.Context, messages *journalStderrQueue) error {
	for {
		message, ok := messages.next(ctx)
		if !ok {
			return nil
		}
		if !r.sendServerMessage(ctx, message) {
			return nil
		}
	}
}

func (r *Reader) sendServerMessage(ctx context.Context, message string) bool {
	if r.serverMessages == nil {
		return true
	}
	select {
	case r.serverMessages <- message:
		return true
	case <-ctx.Done():
		return false
	}
}

func scanLinesPreserveEndings(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF {
		return len(data), nil, nil
	}
	return 0, nil, nil
}
