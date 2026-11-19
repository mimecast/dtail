package handlers

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
)

type readTimings struct {
	globRetryInterval         time.Duration
	readRetryInterval         time.Duration
	outputEOFAckTimeout       time.Duration
	legacyAggregateInputGrace time.Duration
	maxLineLength             int
	maxGlobTargets            int
}

type lineWriterFactory func(context.Context, uint64) LineWriter

type readCommandDependencies struct {
	server          readCommandServer
	lifecycle       readCommandLifecycle
	aggregates      readCommandAggregates
	logger          logging.Logger
	readerLogger    logging.Logger
	logContext      any
	timings         readTimings
	newLineWriter   lineWriterFactory
	abortAfterPanic func()
	serverless      bool
	// readHub shares follow reads between sessions; nil reads privately.
	readHub *readhub.Hub
}

type readCommandDependencyProvider interface {
	readCommandDependencies() readCommandDependencies
}

type readCommandAggregates interface {
	Aggregate() *mapaggregate.Aggregate
}

type readCommandLifecycle interface {
	CompletePendingFile() (remaining int32, activeCommands int32)
	PendingAndActive() (pending int32, activeCommands int32)
	TriggerShutdown(context.Context)
	DebugReadLifecycle(message string, args ...any)
}

// readCommandServer exposes only the handler operations that a read command
// drives directly. Logging, aggregate completion, writer creation, and timing
// are injected as separate values or focused collaborators.
type readCommandServer interface {
	// PrepareReadTarget validates the session's access to path (see
	// sessionuser.User.ResolveReadTarget for its errors).
	PrepareReadTarget(path string) (fs.ValidatedReadTarget, error)
	AcquireReadSlot(context.Context, omode.Mode, string) (release func(), acquired bool)
	TryAcquireReadSlot(omode.Mode, string) (release func(), acquired bool)
	SendReadMessage(context.Context, uint64, string)
	NewReadMessages(context.Context, uint64) (chan string, func())
	AddPendingFiles(delta int32) int32
	PendingAndActive() (pending int32, activeCommands int32)
	FinishReadBatch(context.Context, omode.Mode, uint64)
}

// readBatchCompletionState is the handler-internal seam for the EOF handshake.
// Keeping the orchestration behind this focused interface lets the ordering of
// epoch capture and the pending-work check be pinned by a deterministic test.
type readBatchCompletionState interface {
	DirectOutputActive() bool
	HasOutputEOF() bool
	OutputEpoch() uint64
	PendingAndActive() (pending int32, activeCommands int32)
	traceSkippedReadBatchEOF(int32, int32)
	flushReadBatch(context.Context, uint64) bool
	SignalOutputEOF(uint64)
	waitForReadBatchAck(context.Context)
}

var _ readCommandServer = (*ServerHandler)(nil)
var _ readCommandLifecycle = (*ServerHandler)(nil)
var _ readCommandAggregates = (*ServerHandler)(nil)
var _ readCommandDependencyProvider = (*ServerHandler)(nil)

func newReadTimings(serverCfg *config.ServerConfig) readTimings {
	if serverCfg == nil {
		serverCfg = &config.ServerConfig{}
	}
	return readTimings{
		globRetryInterval:         durationFromMilliseconds(serverCfg.ReadGlobRetryIntervalMs, 5*time.Second),
		readRetryInterval:         durationFromMilliseconds(serverCfg.ReadRetryIntervalMs, 2*time.Second),
		outputEOFAckTimeout:       durationFromMilliseconds(serverCfg.OutputEOFAckTimeoutMs, 2*time.Second),
		legacyAggregateInputGrace: legacyUnbatchedAggregateInputGrace,
		maxLineLength:             positiveIntOrDefault(serverCfg.MaxLineLength, 1024*1024),
		maxGlobTargets:            positiveIntOrDefault(serverCfg.MaxGlobTargets, 1000),
	}
}

func (t readTimings) withDefaults() readTimings {
	if t.globRetryInterval <= 0 {
		t.globRetryInterval = 5 * time.Second
	}
	if t.readRetryInterval <= 0 {
		t.readRetryInterval = 2 * time.Second
	}
	if t.outputEOFAckTimeout <= 0 {
		t.outputEOFAckTimeout = 2 * time.Second
	}
	if t.legacyAggregateInputGrace <= 0 {
		t.legacyAggregateInputGrace = legacyUnbatchedAggregateInputGrace
	}
	if t.maxLineLength <= 0 {
		t.maxLineLength = 1024 * 1024
	}
	if t.maxGlobTargets <= 0 {
		t.maxGlobTargets = 1000
	}
	return t
}

// PrepareReadTarget validates the current user's access to the given path.
func (h *ServerHandler) PrepareReadTarget(path string) (fs.ValidatedReadTarget, error) {
	return h.user.ResolveReadTarget(path, "readfiles")
}

// ServerlessOutput returns the in-process output destination the runtime
// handed over. It is nil exactly when this handler is not serverless; there
// is no stdout default, so a handler can never fall back to writing the
// session payload into its own process output.
func (h *ServerHandler) ServerlessOutput() io.Writer {
	return h.serverlessOutput
}

// AcquireReadSlot waits for the concurrency slot associated with mode and
// returns an idempotent release function when the slot is acquired.
func (h *ServerHandler) AcquireReadSlot(ctx context.Context, mode omode.Mode, path string) (func(), bool) {
	limiter := h.readLimiter(mode)

	select {
	case limiter <- struct{}{}:
		h.Logger().Debug(h.user, "Got limiter slot immediately", "path", path)
	case <-ctx.Done():
		h.Logger().Debug(h.user, "Context cancelled while waiting for limiter", "path", path)
		return nil, false
	default:
		h.Logger().Info(h.user, "Server limit hit, queueing file", "limiterLen", len(limiter), "path", path, "maxConcurrent", cap(limiter))
		select {
		case limiter <- struct{}{}:
			h.Logger().Info(h.user, "Server limit OK now, processing file", "limiterLen", len(limiter), "path", path)
		case <-ctx.Done():
			h.Logger().Debug(h.user, "Context cancelled while queued for limiter", "path", path)
			return nil, false
		}
	}
	return releaseReadSlot(limiter), true
}

// TryAcquireReadSlot takes the concurrency slot associated with mode if one is
// free, without waiting, and returns an idempotent release function when the
// slot is acquired.
func (h *ServerHandler) TryAcquireReadSlot(mode omode.Mode, path string) (func(), bool) {
	limiter := h.readLimiter(mode)
	select {
	case limiter <- struct{}{}:
		h.Logger().Debug(h.user, "Got limiter slot immediately", "path", path)
		return releaseReadSlot(limiter), true
	default:
		h.Logger().Debug(h.user, "No free limiter slot", "path", path)
		return nil, false
	}
}

func (h *ServerHandler) readLimiter(mode omode.Mode) chan struct{} {
	if mode == omode.CatClient || mode == omode.GrepClient {
		return h.catLimiter
	}
	return h.tailLimiter
}

// releaseReadSlot returns an idempotent function that gives back a slot of
// limiter.
func releaseReadSlot(limiter chan struct{}) func() {
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			<-limiter
		}
	}
}

// SendReadMessage forwards a generation-bound message to the session output.
func (h *ServerHandler) SendReadMessage(ctx context.Context, generation uint64, message string) {
	select {
	case h.serverMessages <- encodeGeneratedMessage(generation, message+"\n"):
	case <-ctx.Done():
	}
}

// NewReadMessages returns a per-reader message channel and a join function.
// The forwarding goroutine binds each message to the command generation.
func (h *ServerHandler) NewReadMessages(ctx context.Context, generation uint64) (chan string, func()) {
	messages := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverHandlerPanic(h.Logger(), h.user, "server message forwarding", h.abortAfterPanic)
		for {
			select {
			case message, ok := <-messages:
				if !ok {
					return
				}
				select {
				case h.serverMessages <- encodeGeneratedMessage(generation, message):
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return messages, func() {
		close(messages)
		<-done
	}
}

// FinishReadBatch owns the output EOF handshake for a completed read batch.
func (h *ServerHandler) FinishReadBatch(ctx context.Context, mode omode.Mode, generation uint64) {
	finishReadBatch(ctx, mode, generation, h)
}

func finishReadBatch(ctx context.Context, mode omode.Mode, generation uint64,
	state readBatchCompletionState) {

	if !isDirectReadMode(mode) || ctx.Err() != nil || !state.DirectOutputActive() || !state.HasOutputEOF() {
		return
	}

	// Capture the epoch before observing pending work. A joining command bumps
	// the epoch, making a stale signal harmless even while flush blocks.
	epoch := state.OutputEpoch()
	pending, active := state.PendingAndActive()
	if pending != 0 {
		state.traceSkippedReadBatchEOF(pending, active)
		return
	}

	if !state.flushReadBatch(ctx, generation) {
		return
	}
	state.SignalOutputEOF(epoch)
	state.waitForReadBatchAck(ctx)
}

// Aggregate returns the MapReduce aggregate if enabled for the session.
// Uses the atomic accessor to avoid a race with concurrent handleMapCommand writes.
func (h *ServerHandler) Aggregate() *mapaggregate.Aggregate {
	return h.getAggregate()
}

// AddPendingFiles increments or decrements the pending file counter.
func (h *ServerHandler) AddPendingFiles(delta int32) int32 {
	return atomic.AddInt32(&h.pendingFiles, delta)
}

// CompletePendingFile marks one file as completed and returns pending/active counters.
func (h *ServerHandler) CompletePendingFile() (remaining int32, activeCommands int32) {
	remaining = atomic.AddInt32(&h.pendingFiles, -1)
	activeCommands = atomic.LoadInt32(&h.activeCommands)
	return remaining, activeCommands
}

// PendingAndActive returns the current pending file and active command counts.
func (h *ServerHandler) PendingAndActive() (pending int32, activeCommands int32) {
	pending = atomic.LoadInt32(&h.pendingFiles)
	activeCommands = atomic.LoadInt32(&h.activeCommands)
	return pending, activeCommands
}

// TriggerShutdown starts the handler shutdown sequence.
func (h *ServerHandler) TriggerShutdown(ctx context.Context) {
	if h.sessionState.keepAlive() || h.isStopping() {
		return
	}
	h.triggerIdleShutdown(ctx)
}

// DebugReadLifecycle records shutdown-coordination diagnostics without exposing
// the handler's logger or user object to the coordinator.
func (h *ServerHandler) DebugReadLifecycle(message string, args ...any) {
	values := make([]any, 0, len(args)+2)
	values = append(values, h.user, message)
	values = append(values, args...)
	h.Logger().Debug(values...)
}

func (h *ServerHandler) readCommandDependencies() readCommandDependencies {
	return readCommandDependencies{
		server:          h,
		lifecycle:       h,
		aggregates:      h,
		logger:          h.Logger(),
		readerLogger:    h.ReaderLogger(),
		logContext:      h.user,
		timings:         h.readTimings.withDefaults(),
		newLineWriter:   h.newReadLineWriter,
		abortAfterPanic: h.abortAfterPanic,
		serverless:      h.serverless,
		readHub:         h.readHub,
	}
}

func (h *ServerHandler) newReadLineWriter(ctx context.Context, generation uint64) LineWriter {
	if h.EnableDirectOutput() {
		h.SendReadMessage(ctx, generation, ".output wake")
	}
	if h.serverless {
		return NewGeneratedDirectWriterWithColorizer(h.ServerlessOutput(), h.hostname, h.plain, true,
			generation, h.sessionState.currentGeneration, h.colorizer)
	}

	writer := NewNetworkWriter(ctx, nil, h.serverMessages, h.hostname, h.plain, false,
		generation, h.sessionState.currentGeneration, h.Logger())
	writer.enqueueOutput = h.EnqueueOutput
	return writer
}

func isDirectReadMode(mode omode.Mode) bool {
	return mode == omode.CatClient || mode == omode.GrepClient || mode == omode.TailClient
}

func (h *ServerHandler) traceSkippedReadBatchEOF(pending, active int32) {
	h.Logger().Trace(h.user, "Skipping output EOF signal for non-final command",
		"pending", pending, "active", active)
}

func (h *ServerHandler) flushReadBatch(ctx context.Context, generation uint64) bool {
	h.Logger().Debug(h.user, "Output mode: flushing data before EOF signal")
	if err := h.flushOutput(ctx); err != nil {
		if ctx.Err() != nil {
			return false
		}
		h.Logger().Error(h.user, "Unable to flush output", err)
		h.reportFlushError(generation, err)
	}
	return true
}

func (h *ServerHandler) waitForReadBatchAck(ctx context.Context) {
	if h.serverless {
		return
	}
	timeout := h.readTimings.withDefaults().outputEOFAckTimeout
	if h.WaitForOutputEOFAck(ctx, timeout) {
		h.Logger().Debug(h.user, "Output EOF handshake released (reader ack or handover)")
		return
	}
	if ctx.Err() != nil {
		return
	}
	h.Logger().Warn(h.user, "Timeout waiting for output EOF acknowledgement",
		"timeout", timeout, "remainingBytes", h.OutputBufferBytes())
}

func durationFromMilliseconds(value int, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

func positiveIntOrDefault(value int, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func (h *ServerHandler) outputManagerConfig() outputManagerConfig {
	return outputManagerConfig{
		bufferMaxBytes:    positiveIntOrDefault(h.serverCfg.OutputBufferMaxBytes, defaultOutputBufferMaxBytes),
		flushTimeout:      durationFromMilliseconds(h.serverCfg.OutputFlushTimeoutMs, defaultOutputFlushTimeout),
		readRetryInterval: durationFromMilliseconds(h.serverCfg.OutputReadRetryIntervalMs, defaultOutputReadRetryInterval),
	}
}
