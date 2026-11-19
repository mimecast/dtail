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
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

const (
	defaultSourceID   = "journal"
	journalctlCommand = "journalctl"
	// maxStderrTokenSize is the longest part of a journalctl stderr line the
	// reader forwards: a longer line is forwarded cut to this length, with a
	// note, and its rest is dropped (see stderrLineSplitter), so it neither
	// fails nor cancels the journalctl run. stdout entries follow
	// MaxLineLength instead (see scanStdout).
	maxStderrTokenSize    = 1024 * 1024
	processTerminateGrace = 200 * time.Millisecond
)

var errStopReading = errors.New("stop journal reading")

// ErrJournalctlNotFound reports that journalctl could not be found on PATH.
var ErrJournalctlNotFound = errors.New("journalctl not found")

// ReaderOptions configures a journalctl-backed reader.
type ReaderOptions struct {
	// Args are the journalctl arguments selecting the entries, e.g. "-u",
	// "ssh.service".
	Args []string
	// SourceID names the source of the lines and of the long-line warning;
	// "journal" when empty.
	SourceID string
	// Follow keeps reading new entries (journalctl -f -n 0); Retry then asks
	// for journalctl to be restarted whenever it exits.
	Follow bool
	// ServerMessages receives journalctl's stderr lines and the long-line
	// warning for the client; nil drops them.
	ServerMessages chan<- string
	// MaxLineLength is the longest entry, in bytes without its "\n", passed on
	// whole (Server.MaxLineLength); a longer one is cut into pieces of exactly
	// this length, as file reads cut their lines (see fs.SplitLine). 0 or
	// less uses the 1 MiB default.
	MaxLineLength int
	// Logger logs the long-line warning; nil logs nothing.
	Logger logging.Logger
}

// Reader reads journal entries by executing journalctl.
type Reader struct {
	journalctlPath    string
	args              []string
	sourceID          string
	serverMessages    chan<- string
	follow            bool
	maxLineLength     int
	logger            logging.Logger
	forwardStderrHook func(context.Context, io.Reader) error
	waitCommandHook   func(*exec.Cmd) error
}

type journalProcess struct {
	cmd          *exec.Cmd
	stdout       io.Reader
	stderr       *io.PipeReader
	stderrWriter *io.PipeWriter
}

type journalStderrWorkers struct {
	drainDone    <-chan error
	deliveryDone <-chan error
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
func NewReader(options ReaderOptions) (*Reader, error) {
	journalctlPath, err := exec.LookPath(journalctlCommand)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournalctlNotFound, err)
	}
	sourceID := options.SourceID
	if sourceID == "" {
		sourceID = defaultSourceID
	}

	return &Reader{
		journalctlPath: journalctlPath,
		args:           append([]string(nil), options.Args...),
		sourceID:       sourceID,
		serverMessages: options.ServerMessages,
		follow:         options.Follow,
		maxLineLength:  options.MaxLineLength,
		logger:         options.Logger,
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
	process, err := r.startJournalProcess(runCtx)
	if err != nil {
		return err
	}
	stderrWorkers := r.startJournalStderrWorkers(runCtx, cancel, process.stderr)

	filter := newJournalFilter(ltx, sink, re, r.sourceID)
	scanErr := runJournalChild("stdout scanning", func() error {
		return r.scanStdout(runCtx, process.stdout, filter, flushLine)
	})
	if scanErr != nil {
		// Stop the child before waiting; WaitDelay kills it if SIGTERM does not
		// make it exit and bounds inherited stderr descriptors.
		cancel()
	}
	waitErr := r.waitForJournalProcess(process.cmd)
	if errors.Is(waitErr, fs.ErrReaderWorkerPanic) {
		// A recovered Wait panic is fatal. Cancel before joining the stderr
		// forwarder so a full server-message channel cannot strand it.
		cancel()
	}
	// cmd.Stderr is an io.Writer, so Wait owns and joins the child-side copy.
	// Closing our writer after that copy finishes publishes EOF to the line
	// forwarder. Unlike StderrPipe, WaitDelay can close the os/exec-owned pipe
	// if a descendant inherits stderr and keeps it open.
	_ = process.stderrWriter.Close()
	stderrErr := stderrWorkers.join()
	filter.Close()
	return journalRunResult(ctx, scanErr, stderrErr, waitErr)
}

func (r *Reader) startJournalProcess(ctx context.Context) (*journalProcess, error) {
	cmd := r.command(ctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open journalctl stdout: %w", err)
	}
	stderr, stderrWriter := io.Pipe()
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("start journalctl: %w", err)
	}
	return &journalProcess{
		cmd:          cmd,
		stdout:       stdout,
		stderr:       stderr,
		stderrWriter: stderrWriter,
	}, nil
}

func (r *Reader) startJournalStderrWorkers(ctx context.Context, cancel context.CancelFunc,
	stderr *io.PipeReader) *journalStderrWorkers {
	queue := newJournalStderrQueue()
	drainDone := make(chan error, 1)
	forwardStderr := func(ctx context.Context, stderr io.Reader) error {
		return scanJournalStderr(ctx, stderr, queue.push)
	}
	if r.forwardStderrHook != nil {
		forwardStderr = r.forwardStderrHook
	}
	go func() {
		defer func() { _ = stderr.Close() }()
		stderrErr := runJournalChild("stderr forwarding", func() error {
			return forwardStderr(ctx, stderr)
		})
		queue.close()
		if stderrErr != nil {
			cancel()
		}
		drainDone <- stderrErr
	}()

	deliveryDone := make(chan error, 1)
	go func() {
		stderrErr := runJournalChild("stderr delivery", func() error {
			return r.deliverJournalStderr(ctx, queue)
		})
		if stderrErr != nil {
			cancel()
		}
		deliveryDone <- stderrErr
	}()
	return &journalStderrWorkers{drainDone: drainDone, deliveryDone: deliveryDone}
}

func (r *Reader) waitForJournalProcess(cmd *exec.Cmd) error {
	waitCommand := cmd.Wait
	if r.waitCommandHook != nil {
		waitCommand = func() error { return r.waitCommandHook(cmd) }
	}
	return waitForJournalctlWith(cmd, waitCommand)
}

func (w *journalStderrWorkers) join() error {
	return errors.Join(<-w.drainDone, <-w.deliveryDone)
}

func journalRunResult(ctx context.Context, scanErr, stderrErr, waitErr error) error {
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

// scanStdout feeds the entries journalctl writes to stdout, one line each
// with its newline, to filter. Like a snapshot file read, it cuts an entry
// longer than the line limit into pieces of exactly that length and warns the
// client about it (see journalLineSplitter), so a long entry never ends the
// read. The scanner starts in the pooled buffer (1 MiB + 1 byte) and only
// grows, up to fs.ScannerTokenLimit, for an entry longer than that, which
// needs a MaxLineLength above 1 MiB. Each journalctl run (a follow read
// restarts it) scans with a splitter of its own: no entry spans two runs.
func (r *Reader) scanStdout(ctx context.Context, stdout io.Reader, filter *journalFilter,
	flushLine func() error) error {

	scanner := bufio.NewScanner(stdout)
	bufPtr := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(bufPtr)

	splitter := r.newLineSplitter()
	scanner.Buffer(*bufPtr, fs.ScannerTokenLimit(splitter.limit))
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		return splitter.split(ctx, data, atEOF)
	})

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

// scanJournalStderr emits journalctl's stderr lines as server messages until
// stderr ends. It holds a pooled scanner buffer (1 MiB + 1 byte) for the whole
// journalctl run, as scanStdout does. A line longer than maxStderrTokenSize is
// emitted cut to that length with a note and the rest of it is dropped,
// instead of failing the scan with bufio.ErrTooLong, which would cancel the
// run.
func scanJournalStderr(ctx context.Context, stderr io.Reader, emit func(string)) error {
	scanner := bufio.NewScanner(stderr)
	bufPtr := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(bufPtr)

	splitter := &stderrLineSplitter{limit: maxStderrTokenSize}
	// limit+1 (the size of the pooled buffer) lets a line of exactly
	// maxStderrTokenSize bytes reach split together with its newline, so it
	// is forwarded whole, not marked truncated (see stderrLineSplitter.split).
	scanner.Buffer(*bufPtr, maxStderrTokenSize+1)
	scanner.Split(splitter.split)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		emit(stderrMessage(scanner.Bytes(), splitter.truncated))
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

// newLineSplitter returns the splitter of one scan of journalctl's stdout,
// which warns the client through the reader's server messages.
func (r *Reader) newLineSplitter() *journalLineSplitter {
	return &journalLineSplitter{
		limit: fs.LineLimit(r.maxLineLength),
		warn: func(ctx context.Context) bool {
			return fs.SendLongLineWarning(ctx, r.logger, r.sourceID, r.serverMessages)
		},
	}
}

// journalLineSplitter splits journalctl's stdout into entries for
// bufio.Scanner, preserving their line endings, with fs.SplitLine: an entry
// of up to limit bytes before its newline is one token, a longer one is cut
// into tokens of exactly limit bytes, after a long-line warning (once per
// sequence of cut pieces, as in file reads).
type journalLineSplitter struct {
	limit  int
	warned bool
	warn   func(context.Context) bool
}

// split is the bufio.SplitFunc of the splitter. It drops an unterminated last
// entry, which only a journalctl stopped in the middle of writing it leaves
// (journalctl ends every entry with a newline), as the reader always did; the
// pieces cut off a long one before are passed on. It returns the context's
// error when the reader is canceled during a long-line warning.
func (s *journalLineSplitter) split(ctx context.Context, data []byte, atEOF bool) (int, []byte, error) {
	advance, token, end := fs.SplitLine(data, atEOF, s.limit)
	switch end {
	case fs.LineEndCut:
		if !s.warned {
			if !s.warn(ctx) {
				return 0, nil, ctx.Err()
			}
			s.warned = true
		}
	case fs.LineEndNewline:
		// A whole entry within the limit ends a sequence of cut pieces, so
		// the next long entry warns again.
		s.warned = false
	case fs.LineEndEOF:
		return advance, nil, nil
	}
	return advance, token, nil
}
