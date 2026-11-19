package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

const networkWriterBufferSize = 64 * 1024

type serverMessageSink func(string) error

// LineWriter defines the interface for direct writing in output mode
type LineWriter interface {
	// WriteLineData writes formatted line data directly to output
	WriteLineData(lineContent []byte, lineNum uint64, sourceID string) error
	// WriteServerMessage writes a server message
	WriteServerMessage(message string) error
	// Flush ensures all buffered data is written
	Flush() error
}

// DirectWriter implements LineWriter for direct network writing
type DirectWriter struct {
	writer           io.Writer
	lineFormatter    lineFormatter
	messageSink      serverMessageSink
	flushDestination func() error
	generation       uint64

	// Buffering for efficiency
	writeBuf bytes.Buffer
	bufSize  int
	mutex    sync.Mutex

	// Stats
	linesWritten uint64
	bytesWritten uint64

	activeGeneration func() uint64
}

var _ LineWriter = (*DirectWriter)(nil)

// NewDirectWriter creates a new output writer
func NewDirectWriter(writer io.Writer, hostname string, plain, serverless bool) *DirectWriter {
	return NewDirectWriterWithColorizer(writer, hostname, plain, serverless, nil)
}

// NewDirectWriterWithColorizer creates an output writer with an injected terminal colorizer.
func NewDirectWriterWithColorizer(writer io.Writer, hostname string, plain, serverless bool,
	colorizer Colorizer) *DirectWriter {
	format := lineFormatProtocol
	switch {
	case serverless && plain:
		format = lineFormatNewline
	case serverless:
		format = lineFormatColored
	case plain:
		format = lineFormatNewline
	}

	w := newDirectWriter(writer, newLineFormatter(format, hostname, colorizer))
	if serverless {
		w.messageSink = discardServerMessage
		if flusher, ok := writer.(interface{ Flush() error }); ok {
			w.flushDestination = flusher.Flush
		}
	} else {
		w.messageSink = func(message string) error {
			return w.writeServerMessage(hostname, plain, message)
		}
	}
	return w
}

func newDirectWriter(writer io.Writer, formatter lineFormatter) *DirectWriter {
	return &DirectWriter{
		writer:        writer,
		lineFormatter: formatter,
		messageSink:   discardServerMessage,
		bufSize:       networkWriterBufferSize,
	}
}

// NewGeneratedDirectWriter creates a DirectWriter bound to a session generation.
func NewGeneratedDirectWriter(writer io.Writer, hostname string, plain, serverless bool, generation uint64, activeGeneration func() uint64) *DirectWriter {
	return NewGeneratedDirectWriterWithColorizer(writer, hostname, plain, serverless,
		generation, activeGeneration, nil)
}

// NewGeneratedDirectWriterWithColorizer creates a generation-bound writer with an injected colorizer.
func NewGeneratedDirectWriterWithColorizer(writer io.Writer, hostname string, plain, serverless bool,
	generation uint64, activeGeneration func() uint64, colorizer Colorizer) *DirectWriter {
	w := NewDirectWriterWithColorizer(writer, hostname, plain, serverless, colorizer)
	w.generation = generation
	w.activeGeneration = activeGeneration
	return w
}

// WriteLineData writes formatted line data directly to output.
func (w *DirectWriter) WriteLineData(lineContent []byte, lineNum uint64, sourceID string) error {
	if !shouldWriteGeneration(w.generation, w.activeGeneration) {
		return nil
	}
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// writeBuf accumulates lines until bufSize before flushing, so record its
	// length before appending: the bytesWritten stat must count only this
	// line's formatted bytes, not the whole buffered backlog again.
	bufLenBefore := w.writeBuf.Len()
	w.lineFormatter(&w.writeBuf, lineContent, lineNum, sourceID)

	// Update stats: add only the delta appended for this line.
	w.linesWritten++
	w.bytesWritten += uint64(w.writeBuf.Len() - bufLenBefore)

	// Buffer writes for better performance - only flush when buffer is full.
	if w.writeBuf.Len() >= w.bufSize {
		return w.flushBuffer()
	}

	return nil
}

// WriteServerMessage writes a server message
func (w *DirectWriter) WriteServerMessage(message string) error {
	if !shouldWriteGeneration(w.generation, w.activeGeneration) {
		return nil
	}
	return w.messageSink(message)
}

// Flush ensures all buffered data is written
func (w *DirectWriter) Flush() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// Force flush any remaining data.
	err := w.flushBuffer()
	if w.flushDestination != nil {
		err = errors.Join(err, w.flushDestination())
	}

	return err
}

func (w *DirectWriter) writeServerMessage(hostname string, plain bool, message string) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if plain && (message == "" || message == "\n") {
		return nil
	}

	kind := protocol.MessageServer
	if plain || len(message) > 0 && message[0] == '.' {
		kind = protocol.MessagePlain
	}
	protocol.EncodeMessage(&w.writeBuf, protocol.Message{
		Kind:     kind,
		Hostname: hostname,
		Content:  message,
	})
	return w.flushBuffer()
}

// flushBuffer writes the buffer content to the writer (must be called with mutex held)
func (w *DirectWriter) flushBuffer() error {
	if w.writeBuf.Len() == 0 {
		return nil
	}

	data := w.writeBuf.Bytes()

	for len(data) > 0 {
		n, err := w.writer.Write(data)
		if err != nil {
			w.writeBuf.Reset()
			return err
		}
		if n <= 0 {
			w.writeBuf.Reset()
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	w.writeBuf.Reset()

	return nil
}

// Stats returns writing statistics
func (w *DirectWriter) Stats() (linesWritten, bytesWritten uint64) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	return w.linesWritten, w.bytesWritten
}

// NetworkWriter writes directly to the network connection bypassing channels
type NetworkWriter struct {
	logger        logging.Logger
	outputLines   chan<- []byte
	enqueueOutput func(context.Context, uint64, []byte, func() uint64) error
	lineFormatter lineFormatter
	messageSink   serverMessageSink
	generation    uint64
	ctx           context.Context

	// Internal buffer for batching writes
	writeBuf    bytes.Buffer
	bufSize     int
	mutex       sync.Mutex
	sendStateCh chan struct{}
	sending     bool

	// Stats
	linesWritten uint64
	bytesWritten uint64

	activeGeneration func() uint64
}

var _ LineWriter = (*NetworkWriter)(nil)

// NewNetworkWriter creates a NetworkWriter that batches formatted
// lines into a 64KB buffer before sending them to the output channel.
//
// bufSize is the field the previous bare struct literal in makeWriter
// omitted. With bufSize left at its zero value the flush condition in
// WriteLineData (writeBuf.Len() < w.bufSize) was never true off the
// backpressure path, so every line was sent as its own output-channel payload —
// one SSH packet and one write syscall per line. Setting bufSize to 64KB (the
// same value NewDirectWriter uses) lets many
// lines coalesce into a single send, which is the whole point of output output.
//
// Follow-mode (dtail tail) latency is preserved: tailWithProcessorOptimized
// calls processor.Flush() after every read chunk, and Flush drains the partial
// writeBuf to the output channel promptly, so batching never holds interactive
// output back past a read boundary.
func NewNetworkWriter(ctx context.Context, outputLines chan<- []byte,
	serverMessages chan<- string, hostname string, plain, serverless bool,
	generation uint64, activeGeneration func() uint64, logger logging.Logger) *NetworkWriter {
	format := lineFormatProtocol
	if plain || serverless {
		format = lineFormatDelimited
	}
	w := newNetworkWriter(ctx, outputLines, newLineFormatter(format, hostname, nil), generation,
		activeGeneration, logger)
	if serverless || serverMessages == nil {
		w.messageSink = discardServerMessage
	} else {
		w.messageSink = func(message string) error {
			select {
			case serverMessages <- encodeGeneratedMessage(generation, message):
				return nil
			default:
				return fmt.Errorf("server message channel full")
			}
		}
	}
	return w
}

func newNetworkWriter(ctx context.Context, outputLines chan<- []byte, formatter lineFormatter,
	generation uint64, activeGeneration func() uint64, logger logging.Logger) *NetworkWriter {
	if ctx == nil {
		panic("handlers: nil network writer context")
	}
	return &NetworkWriter{
		logger:           logging.OrNop(logger),
		outputLines:      outputLines,
		lineFormatter:    formatter,
		messageSink:      discardServerMessage,
		generation:       generation,
		ctx:              ctx,
		bufSize:          networkWriterBufferSize,
		sendStateCh:      make(chan struct{}),
		activeGeneration: activeGeneration,
	}
}

func (w *NetworkWriter) log() logging.Logger {
	if w.logger == nil {
		return logging.NopLogger{}
	}
	return w.logger
}

// WriteLineData formats and writes line data directly to the output channel.
// Builds the protocol-formatted line and sends it via sendToChannel.
func (w *NetworkWriter) WriteLineData(lineContent []byte, lineNum uint64, sourceID string) error {
	if !shouldWriteGeneration(w.generation, w.activeGeneration) {
		return nil
	}
	w.mutex.Lock()

	// Per-line hot path (server mode): gate both traces so their uint64/int/string
	// args are not boxed on every line when trace is off. Evaluated once so the
	// second trace below shares the same decision.
	traceEnabled := w.log().TraceEnabled()
	if traceEnabled {
		writerTrace(w.log(), "NetworkWriter.WriteLineData", "lineNum", lineNum, "sourceID", sourceID, "contentLen", len(lineContent))
	}

	// writeBuf accumulates lines until bufSize before flushing, so record its
	// length before appending: the bytesWritten stat must count only this
	// line's formatted bytes, not the whole buffered backlog again.
	bufLenBefore := w.writeBuf.Len()

	w.lineFormatter(&w.writeBuf, lineContent, lineNum, sourceID)

	// Update stats: add only the delta appended for this line.
	w.linesWritten++
	w.bytesWritten += uint64(w.writeBuf.Len() - bufLenBefore)

	if traceEnabled {
		writerTrace(w.log(), "NetworkWriter.WriteLineData", "linesWritten", w.linesWritten, "bytesWritten", w.bytesWritten, "bufSize", w.writeBuf.Len())
	}

	if w.writeBuf.Len() < w.bufSize || w.sending {
		w.mutex.Unlock()
		return nil
	}

	data := append([]byte(nil), w.writeBuf.Bytes()...)
	w.writeBuf.Reset()
	w.markSendingLocked()
	w.mutex.Unlock()

	return w.sendBufferedData(data)
}

// sendBufferedData sends buffered data to the output channel while tracking the
// in-flight send state so Flush can wait for completion without holding the mutex.
func (w *NetworkWriter) sendBufferedData(data []byte) error {
	defer w.finishSending()
	return w.sendToChannel(data)
}

func (w *NetworkWriter) ensureSendStateChLocked() {
	if w.sendStateCh == nil {
		w.sendStateCh = make(chan struct{})
	}
}

func (w *NetworkWriter) markSendingLocked() {
	w.ensureSendStateChLocked()
	w.sending = true
}

func (w *NetworkWriter) finishSending() {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if !w.sending {
		return
	}

	oldCh := w.sendStateCh
	w.sendStateCh = make(chan struct{})
	w.sending = false

	if oldCh != nil {
		close(oldCh)
	}
}

func (w *NetworkWriter) waitForSendAvailability(ctx context.Context) error {
	for {
		w.mutex.Lock()
		if !w.sending {
			w.mutex.Unlock()
			return nil
		}

		stateCh := w.sendStateCh
		w.mutex.Unlock()

		select {
		case <-stateCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendToChannel sends buffered data to the output channel.
// It blocks only on the channel send itself and exits promptly when the
// request context is canceled.
func (w *NetworkWriter) sendToChannel(data []byte) error {
	// Per-send path (once per 64KB buffer flush): decide once so none of the
	// diagnostic traces below build a []any or box their args when trace
	// is off. Cheaper and keeps the send loop tight.
	traceEnabled := w.log().TraceEnabled()

	if !shouldWriteGeneration(w.generation, w.activeGeneration) {
		if traceEnabled {
			writerTrace(w.log(), "NetworkWriter.sendToChannel", "generation became stale before send")
		}
		return nil
	}

	ctx := w.ctx
	if w.enqueueOutput != nil {
		return w.enqueueOutput(ctx, w.generation, data, w.activeGeneration)
	}
	if w.outputLines == nil {
		if traceEnabled {
			writerTrace(w.log(), "NetworkWriter.sendToChannel", "outputLines channel is nil")
		}
		return nil
	}

	encoded := encodeGeneratedBytes(w.generation, data)

	if traceEnabled {
		writerTrace(w.log(), "NetworkWriter.sendToChannel", "sending to outputLines channel", "dataLen", len(data))
	}

	if err := ctx.Err(); err != nil {
		if traceEnabled {
			writerTrace(w.log(), "NetworkWriter.sendToChannel", "context already cancelled before send", "err", err)
		}
		return err
	}

	for {
		if !shouldWriteGeneration(w.generation, w.activeGeneration) {
			if traceEnabled {
				writerTrace(w.log(), "NetworkWriter.sendToChannel", "generation became stale while waiting to retry send")
			}
			return nil
		}

		// Channel writability and cancellation are explicit wakeups. The timer is
		// only a compatibility fallback for callers that expose generation as a
		// callback rather than a cancellation signal.
		var generationTimer *time.Timer
		var generationTimerC <-chan time.Time
		if w.activeGeneration != nil {
			generationTimer = time.NewTimer(minimumOutputGenerationSafetyInterval)
			generationTimerC = generationTimer.C
		}
		select {
		case w.outputLines <- encoded:
			stopOptionalTimer(generationTimer)
			if traceEnabled {
				writerTrace(w.log(), "NetworkWriter.sendToChannel", "sent to channel successfully")
			}
			return nil
		case <-ctx.Done():
			stopOptionalTimer(generationTimer)
			if traceEnabled {
				writerTrace(w.log(), "NetworkWriter.sendToChannel", "context cancelled while waiting to send")
			}
			return ctx.Err()
		case <-generationTimerC:
		}
	}
}

// WriteServerMessage writes a server message
func (w *NetworkWriter) WriteServerMessage(message string) error {
	if !shouldWriteGeneration(w.generation, w.activeGeneration) {
		return nil
	}
	return w.messageSink(message)
}

// Flush ensures all data is written
func (w *NetworkWriter) Flush() error {
	writerTrace(w.log(), "NetworkWriter.Flush", "called")

	ctx := w.ctx

	for {
		if err := w.waitForSendAvailability(ctx); err != nil {
			return err
		}

		w.mutex.Lock()
		if w.sending {
			w.mutex.Unlock()
			continue
		}
		if w.writeBuf.Len() == 0 {
			w.mutex.Unlock()
			break
		}

		writerTrace(w.log(), "NetworkWriter.Flush", "flushing buffered data", "bufSize", w.writeBuf.Len())

		data := append([]byte(nil), w.writeBuf.Bytes()...)
		w.writeBuf.Reset()
		w.markSendingLocked()
		w.mutex.Unlock()

		if err := w.sendBufferedData(data); err != nil {
			return err
		}
		writerTrace(w.log(), "NetworkWriter.Flush", "flushed data to channel")
	}

	writerTrace(w.log(), "NetworkWriter.Flush", "completed")

	return nil
}

// Stats returns writing statistics
func (w *NetworkWriter) Stats() (linesWritten, bytesWritten uint64) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	return w.linesWritten, w.bytesWritten
}

// writerTrace forwards to the injected logger for the cold/low-frequency
// output writer paths (Flush/Close, per-64KB sends). TraceEnabled() is nil-safe
// and also skips the work when trace is off. Per-line hot callers
// (DirectLineProcessor.ProcessLine, NetworkWriter.WriteLineData) must wrap
// their call in an explicit TraceEnabled check so the variadic
// slice and argument boxing are elided at the call site, not merely here.
func writerTrace(logger logging.Logger, args ...any) {
	if !logger.TraceEnabled() {
		return
	}
	logger.Trace(args...)
}

func shouldWriteGeneration(generation uint64, activeGeneration func() uint64) bool {
	if generation == 0 || activeGeneration == nil {
		return true
	}

	currentGeneration := activeGeneration()
	if currentGeneration == 0 {
		return true
	}

	return currentGeneration == generation
}

func discardServerMessage(string) error {
	return nil
}

// DirectLineProcessor processes lines directly without channels in output mode
type DirectLineProcessor struct {
	writer    LineWriter
	globID    string
	lineCount uint64
	logger    logging.Logger
}

// NewDirectLineProcessor creates a processor that writes directly
func NewDirectLineProcessor(writer LineWriter, globID string, logger logging.Logger) *DirectLineProcessor {
	return &DirectLineProcessor{
		writer: writer,
		globID: globID,
		logger: logging.OrNop(logger),
	}
}

// ProcessLine writes a line directly to the output
func (p *DirectLineProcessor) ProcessLine(lineContent *bytes.Buffer, lineNum uint64, sourceID string) error {
	p.lineCount++

	// Per-line hot path: gate the trace so the uint64/string args are not boxed
	// into a []any on every line when trace logging is off (the default).
	// This call site was ~98% of all allocated objects and ~28% of CPU
	// (convT64+convTstring) in the output serverless dcat profile.
	if p.logger.TraceEnabled() {
		writerTrace(p.logger, "DirectLineProcessor.ProcessLine", "lineCount", p.lineCount, "lineNum", lineNum, "sourceID", sourceID)
	}

	// Write directly to output
	err := p.writer.WriteLineData(lineContent.Bytes(), lineNum, sourceID)

	// Recycle the buffer
	pool.RecycleBytesBuffer(lineContent)

	return err
}

// Flush ensures all data is written
func (p *DirectLineProcessor) Flush() error {
	writerTrace(p.logger, "DirectLineProcessor.Flush", "lineCount", p.lineCount)
	return p.writer.Flush()
}

// Close flushes any remaining data
func (p *DirectLineProcessor) Close() error {
	writerTrace(p.logger, "DirectLineProcessor.Close", "lineCount", p.lineCount)
	return p.writer.Flush()
}
