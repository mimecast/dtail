package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

const (
	defaultOutputBufferMaxBytes    = config.DefaultOutputBufferMaxBytes
	outputQueueEntryBudgetBytes    = 64
	defaultOutputFlushTimeout      = 2 * time.Second
	defaultOutputReadRetryInterval = time.Millisecond
	// Generation changes normally cancel their command context. Keep a bounded
	// fallback for older callers that only expose an activeGeneration callback;
	// clamping it prevents a full output buffer from reintroducing millisecond
	// polling while still releasing a stale writer eventually.
	minimumOutputGenerationSafetyInterval = 100 * time.Millisecond
	defaultOutputEOFAckQuietPeriod        = 50 * time.Millisecond
	defaultOutputEOFAckTimeout            = 2 * time.Second
)

type outputManagerConfig struct {
	bufferMaxBytes    int
	flushTimeout      time.Duration
	readRetryInterval time.Duration
	eofAckQuietPeriod time.Duration
}

type generatedOutput struct {
	generation       uint64
	payload          []byte
	mustFinishRecord bool
	// retainedBytes is the full backing allocation charged to the session.
	// It deliberately remains unchanged while payload is sliced during a
	// partial read, because the unread suffix still retains that allocation.
	retainedBytes int
}

var errOutputPayloadTooLarge = errors.New("output payload exceeds session buffer limit")
var errOutputFlushTimeout = errors.New("timed out flushing output")

// outputManager coordinates output-mode state between command goroutines
// (enable/signalEOF/flush/waitForEOFAck, spawned per command by the server
// handler) and the session output goroutine (io.Copy -> baseHandler.Read ->
// tryRead). These run concurrently, so all mutable state is guarded by mu:
// mode, queue, buffer, eof, eofAck, eofEmptySince and epoch must only be
// accessed while holding mu. The configuration fields (bufferMaxBytes,
// flushTimeout, ...) are
// deliberately not guarded: configure() runs exactly once from the handler
// constructor before any goroutine can touch the manager, so goroutine
// creation establishes the necessary happens-before edge.
type outputManager struct {
	logger logging.Logger
	mu     sync.Mutex
	mode   bool
	queue  []generatedOutput
	buffer generatedOutput
	eof    chan struct{}
	eofAck chan struct{}

	// epoch is bumped by every enable() call and identifies the newest batch
	// that joined the output session. signalEOF only closes the EOF channel
	// when the signaler's captured epoch is still current, so a command that
	// decided "the batch is over" before another command joined cannot EOF
	// the newcomer's output (see signalEOF for the full protocol).
	epoch uint64

	bufferMaxBytes    int
	bufferedBytes     int
	retainedBytes     int
	bufferedEntries   int
	spaceAvailable    chan struct{}
	stateChanged      chan struct{}
	drainDone         chan struct{}
	flushTimeout      time.Duration
	readRetryInterval time.Duration
	eofAckQuietPeriod time.Duration

	eofEmptySince time.Time
}

// configure sets the tunables. It must be called before the manager is used
// concurrently (i.e. from the handler constructor); see the struct comment
// for why the config fields need no locking.
func (t *outputManager) configure(cfg outputManagerConfig, logger logging.Logger) {
	t.logger = logging.OrNop(logger)
	if cfg.bufferMaxBytes > 0 {
		t.bufferMaxBytes = cfg.bufferMaxBytes
	}
	if cfg.flushTimeout > 0 {
		t.flushTimeout = cfg.flushTimeout
	}
	if cfg.readRetryInterval > 0 {
		t.readRetryInterval = cfg.readRetryInterval
	}
	if cfg.eofAckQuietPeriod > 0 {
		t.eofAckQuietPeriod = cfg.eofAckQuietPeriod
	}
}

func (t *outputManager) log() logging.Logger {
	if t.logger == nil {
		return logging.NopLogger{}
	}
	return t.logger
}

func (t *outputManager) resolvedBufferMaxBytes() int {
	if t.bufferMaxBytes > 0 {
		return t.bufferMaxBytes
	}
	return defaultOutputBufferMaxBytes
}

func (t *outputManager) resolvedMaxQueueEntries() int {
	entries := t.resolvedBufferMaxBytes() / outputQueueEntryBudgetBytes
	if entries < 16 {
		return 16
	}
	return entries
}

func (t *outputManager) resolvedFlushTimeout() time.Duration {
	if t.flushTimeout > 0 {
		return t.flushTimeout
	}
	return defaultOutputFlushTimeout
}

func (t *outputManager) resolvedReadRetryInterval() time.Duration {
	if t.readRetryInterval > 0 {
		return t.readRetryInterval
	}
	return defaultOutputReadRetryInterval
}

func (t *outputManager) resolvedEOFAckQuietPeriod() time.Duration {
	if t.eofAckQuietPeriod > 0 {
		return t.eofAckQuietPeriod
	}
	return defaultOutputEOFAckQuietPeriod
}

// enable atomically switches output mode on. It returns true when it
// transitioned from disabled to enabled and false when output mode was already
// active. Fresh EOF/EOF-ack channels are created on the off->on transition,
// so a concurrent (or repeated) enable can never yank a live EOF channel out
// from under an in-flight batch.
//
// One already-enabled case still refreshes the handshake channels: when the
// previous batch signaled EOF but the reader never acknowledged it (e.g.
// WaitForOutputEOFAck timed out on a slow client), t.eof is already closed.
// A new batch inheriting that closed channel would be disabled by
// maybeAckEOFLocked as soon as the output buffer is briefly empty, stranding
// the batch's remaining output. Refreshing is safe: the old eofAck is closed
// first, so a goroutine still blocked on it (e.g. the previous batch mid
// quiet-period) is released immediately instead of stalling until its
// timeout — its data was already flushed before it signaled EOF, and the
// reader can never acknowledge a replaced handshake anyway.
//
// Every enable() call — transition, join-while-enabled, or stale refresh —
// bumps the handshake epoch; see signalEOF.
func (t *outputManager) enable() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.epoch++

	if t.mode {
		if t.eofSignaledLocked() {
			// Stale handshake from an unacknowledged previous batch: release
			// its waiter and start a fresh handshake so the new batch is not
			// EOF'd prematurely.
			t.signalEOFAckLocked()
			t.resetEOFHandshakeLocked()
		}
		t.signalStateChangedLocked()
		return false
	}
	t.mode = true
	if t.spaceAvailable == nil {
		t.spaceAvailable = make(chan struct{})
	}
	// New batch of files: new EOF handshake channels.
	t.resetEOFHandshakeLocked()
	t.signalStateChangedLocked()
	return true
}

// currentEpoch returns the handshake epoch to be captured by a command that
// is about to decide whether its batch is over. Capture it BEFORE checking
// the pending-work count: joiners increment the pending count before calling
// enable(), so a joiner that is invisible to a pending==0 check is guaranteed
// to bump the epoch after the capture, invalidating the stale signal.
func (t *outputManager) currentEpoch() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epoch
}

// eofSignaledLocked reports whether the current EOF channel exists and has
// already been closed by signalEOF. The caller must hold t.mu.
func (t *outputManager) eofSignaledLocked() bool {
	if t.eof == nil {
		return false
	}
	select {
	case <-t.eof:
		return true
	default:
		return false
	}
}

// resetEOFHandshakeLocked mints fresh EOF handshake channels for a new batch
// and clears the quiet-period tracking. The caller must hold t.mu.
func (t *outputManager) resetEOFHandshakeLocked() {
	t.eof = make(chan struct{})
	t.eofAck = make(chan struct{})
	t.eofEmptySince = time.Time{}
}

func (t *outputManager) enabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mode
}

func (t *outputManager) hasEOF() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.eof != nil
}

// signalEOF closes the EOF channel once, but only when the given epoch
// (captured via currentEpoch before the caller's pending-work check) is still
// current. If another command joined the output session in between — bumping
// the epoch via enable() — the signal is a stale "batch over" decision that
// predates the newcomer's output, and closing the (possibly shared) EOF
// channel would let the reader disable output mode mid-batch. Such stale
// signals are therefore dropped; the newcomer signals EOF itself when its
// batch drains. The stale signaler's WaitForOutputEOFAck then waits on the
// newcomer's handshake — released when that batch completes, or by timeout.
// Either way its own data was already flushed before it signaled.
//
// Joiners that never signal EOF themselves (output-aggregate/dmap commands,
// which are excluded from the cat/grep/tail EOF epilogue) also bump the
// epoch. A concurrently finishing cat's signal is then dropped and its ack
// wait deterministically times out. That degradation is bounded (one ack
// timeout) and loses no data: the cat's output was flushed before it
// signaled, and session shutdown flushes whatever remains.
func (t *outputManager) signalEOF(epoch uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if epoch != t.epoch {
		// A newer batch joined after the caller captured its epoch.
		return
	}

	if t.eof == nil {
		return
	}

	select {
	case <-t.eof:
		// Already closed
	default:
		close(t.eof)
		t.signalStateChangedLocked()
	}
}

// signalEOFAckLocked closes the EOF-ack channel once. The caller must hold
// t.mu; callers are maybeAckEOFLocked (normal reader-side ack) and enable
// (releasing the previous batch's waiter when refreshing a stale handshake).
func (t *outputManager) signalEOFAckLocked() {
	if t.eofAck == nil {
		return
	}

	select {
	case <-t.eofAck:
		// Already closed.
	default:
		close(t.eofAck)
	}
}

// waitForEOFAck blocks until the reader goroutine acknowledges the EOF, the
// context is cancelled, or the timeout expires. The ack channel is snapshotted
// under the lock and waited on outside it, so a blocked waiter never stalls the
// reader that has to deliver the ack.
//
// A timeout <= 0 is clamped to the default (mirroring the fallback that
// OutputEOFAckTimeout uses) as defense-in-depth: every current handshake
// replacement leaves the old ack channel closed (a completed handshake was
// acked by the reader, and the stale refresh in enable() closes it
// explicitly), so a forever-wait cannot presently hang — but the clamp keeps
// that true for any future replacement path or caller.
func (t *outputManager) waitForEOFAck(ctx context.Context, timeout time.Duration) bool {
	if ctx == nil {
		panic("handlers: nil EOF acknowledgement context")
	}
	t.mu.Lock()
	eofAck := t.eofAck
	t.mu.Unlock()

	if eofAck == nil {
		return true
	}

	if timeout <= 0 {
		timeout = defaultOutputEOFAckTimeout
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-eofAck:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// enqueue adds payload to the session queue without allowing queued and
// partially-read payload bytes to exceed the configured limit. Admission is
// atomic per logical writer payload: an oversized payload is rejected before
// any prefix can reach the client.
func (t *outputManager) enqueue(ctx context.Context, generation uint64, payload []byte,
	activeGeneration func() uint64) error {

	if ctx == nil {
		panic("handlers: nil output enqueue context")
	}
	if len(payload) == 0 {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !shouldWriteGeneration(generation, activeGeneration) {
			return nil
		}

		t.mu.Lock()
		maxBytes := t.resolvedBufferMaxBytes()
		if len(payload) > maxBytes {
			t.mu.Unlock()
			return fmt.Errorf("%w: size %d bytes, limit %d bytes",
				errOutputPayloadTooLarge, len(payload), maxBytes)
		}
		if t.tryEnqueueLocked(generation, payload, maxBytes) {
			t.mu.Unlock()
			return nil
		}

		spaceAvailable := t.spaceAvailableLocked()
		t.mu.Unlock()

		if activeGeneration == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-spaceAvailable:
			}
			continue
		}

		recheckInterval := t.resolvedReadRetryInterval()
		if recheckInterval < minimumOutputGenerationSafetyInterval {
			recheckInterval = minimumOutputGenerationSafetyInterval
		}
		timer := time.NewTimer(recheckInterval)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		case <-spaceAvailable:
			stopTimer(timer)
		case <-timer.C:
		}
	}

}

// tryEnqueueLocked admits a complete logical payload while bounding both the
// unread bytes and the payload backing allocations retained by queue/buffer.
// Adjacent payloads from the same generation share a descriptor. Geometric
// growth avoids copying every preceding tiny write, and all spare capacity is
// charged immediately.
func (t *outputManager) tryEnqueueLocked(generation uint64, payload []byte, maxBytes int) bool {
	last := len(t.queue) - 1
	if last >= 0 && t.queue[last].generation == generation {
		entry := &t.queue[last]
		neededLength := len(entry.payload) + len(payload)
		if neededLength > entry.retainedBytes {
			available := maxBytes - t.retainedBytes
			minimumGrowth := neededLength - entry.retainedBytes
			if minimumGrowth > available {
				return false
			}

			targetCapacity := entry.retainedBytes * 2
			if targetCapacity < neededLength {
				targetCapacity = neededLength
			}
			if maximumCapacity := entry.retainedBytes + available; targetCapacity > maximumCapacity {
				targetCapacity = maximumCapacity
			}
			grown := make([]byte, len(entry.payload), targetCapacity)
			copy(grown, entry.payload)
			entry.payload = grown
			t.retainedBytes += targetCapacity - entry.retainedBytes
			entry.retainedBytes = targetCapacity
		}
		entry.payload = append(entry.payload, payload...)
		t.markNotDrainedLocked()
		t.bufferedBytes += len(payload)
		t.signalStateChangedLocked()
		return true
	}

	if t.bufferedEntries >= t.resolvedMaxQueueEntries() || t.retainedBytes+len(payload) > maxBytes {
		return false
	}
	queuedPayload := make([]byte, len(payload))
	copy(queuedPayload, payload)
	t.queue = append(t.queue, generatedOutput{
		generation:    generation,
		payload:       queuedPayload,
		retainedBytes: len(queuedPayload),
	})
	t.markNotDrainedLocked()
	t.bufferedBytes += len(payload)
	t.retainedBytes += len(queuedPayload)
	t.bufferedEntries++
	t.signalStateChangedLocked()
	return true
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func stopOptionalTimer(timer *time.Timer) {
	if timer != nil {
		stopTimer(timer)
	}
}

func (t *outputManager) bufferedLen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bufferedBytes
}

func (t *outputManager) spaceAvailableLocked() <-chan struct{} {
	if t.spaceAvailable == nil {
		t.spaceAvailable = make(chan struct{})
	}
	return t.spaceAvailable
}

// readWait snapshots the state-change channel before Read inspects the queue.
// A producer sends to that channel after changing output state, so an arrival
// between this snapshot and tryRead cannot be lost.
func (t *outputManager) readWait() (<-chan struct{}, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stateChanged == nil {
		t.stateChanged = make(chan struct{}, 1)
	}
	// Clear the notification whose state tryRead is about to inspect. A producer
	// racing after this drain leaves a token for the blocking select, while a
	// producer racing before it has already made its state visible under mu.
	select {
	case <-t.stateChanged:
	default:
	}

	var eofWait time.Duration
	if !t.eofEmptySince.IsZero() {
		eofWait = time.Until(t.eofEmptySince.Add(t.resolvedEOFAckQuietPeriod()))
		if eofWait <= 0 {
			eofWait = time.Nanosecond
		}
	}
	return t.stateChanged, eofWait
}

func (t *outputManager) signalStateChangedLocked() {
	if t.stateChanged == nil {
		t.stateChanged = make(chan struct{}, 1)
	}
	select {
	case t.stateChanged <- struct{}{}:
	default:
	}
}

func (t *outputManager) markNotDrainedLocked() {
	if t.bufferedBytes == 0 && t.drainDone == nil {
		t.drainDone = make(chan struct{})
	}
}

func (t *outputManager) releaseOutputLocked(payloadBytes, retainedBytes int, entry bool) {
	if payloadBytes <= 0 && retainedBytes <= 0 && !entry {
		return
	}

	t.bufferedBytes -= payloadBytes
	if t.bufferedBytes < 0 {
		t.bufferedBytes = 0
	}
	t.retainedBytes -= retainedBytes
	if t.retainedBytes < 0 {
		t.retainedBytes = 0
	}
	if entry && t.bufferedEntries > 0 {
		t.bufferedEntries--
	}
	if t.bufferedBytes == 0 && t.drainDone != nil {
		close(t.drainDone)
		t.drainDone = nil
	}
	if (retainedBytes > 0 || entry) && t.spaceAvailable != nil {
		close(t.spaceAvailable)
		t.spaceAvailable = make(chan struct{})
	}
}

// flush waits for the reader to consume all buffered output. The drain channel
// belongs to one nonempty interval and is closed exactly on its transition back
// to empty, which avoids both polling and lost wakeups.
func (t *outputManager) flush(ctx context.Context, user *user.User) error {
	if ctx == nil {
		panic("handlers: nil output flush context")
	}

	t.mu.Lock()
	remaining := t.bufferedBytes
	drainDone := t.drainDone
	if remaining > 0 && drainDone == nil {
		drainDone = make(chan struct{})
		t.drainDone = drainDone
	}
	t.mu.Unlock()
	t.log().Debug(user, "Flushing output data", "bufferedBytes", remaining)
	if remaining == 0 || drainDone == nil {
		return nil
	}

	timer := time.NewTimer(t.resolvedFlushTimeout())
	defer timer.Stop()
	select {
	case <-drainDone:
		t.log().Debug(user, "Output buffer drained successfully")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("flush output: %w", ctx.Err())
	case <-timer.C:
		remaining = t.bufferedLen()
		if remaining == 0 {
			return nil
		}
		return fmt.Errorf("%w after %s: %d bytes remain",
			errOutputFlushTimeout, t.resolvedFlushTimeout(), remaining)
	}
}

// tryRead tries to serve data from output state and channels.
// Returns handled=false when caller should continue with normal path.
//
// It runs on the session output goroutine and holds t.mu for its entire
// duration, so command goroutines calling enable/signalEOF/bufferedLen never
// observe torn state.
//
// Lock ordering: the shouldDropGeneration callback is invoked (via
// consumeLocked) while t.mu is held and itself acquires sessionState.mu
// (sessionCommandState.currentGeneration), establishing the ordering
// output.mu -> sessionState.mu. Nothing may call into outputManager while
// holding sessionState.mu, or it would deadlock.
func (t *outputManager) tryRead(p []byte, user *user.User, shouldDropGeneration func(uint64) bool) (n int, handled bool) {
	// tryRead runs on the session output goroutine once per Read (i.e. per output
	// payload / ~64KB in server mode). Decide trace state once, before taking
	// the lock, so none of the per-read diagnostics below box their int/string
	// args or build a []any when trace is off (the default). This also
	// shortens the t.mu hold time. Locking semantics are unchanged: the guard is
	// a pure branch and touches no lock. maxLevel is fixed at logger construction.
	traceEnabled := t.log().TraceEnabled()

	t.mu.Lock()
	defer t.mu.Unlock()

	// Drain buffered remainder data first. On a generation change, complete
	// only a protocol record whose prefix was already sent; later complete
	// records in the same writer batch are stale and are discarded.
	if len(t.buffer.payload) > 0 {
		if shouldDropGeneration != nil && shouldDropGeneration(t.buffer.generation) {
			if !t.buffer.mustFinishRecord {
				t.releaseOutputLocked(len(t.buffer.payload), t.buffer.retainedBytes, true)
				t.buffer = generatedOutput{}
			} else if delimiter := bytes.IndexByte(t.buffer.payload, protocol.MessageDelimiter); delimiter >= 0 {
				commitLength := delimiter + 1
				n = copy(p, t.buffer.payload[:commitLength])
				t.buffer.payload = t.buffer.payload[n:]
				t.releaseOutputLocked(n, 0, false)
				if n == commitLength {
					t.releaseOutputLocked(len(t.buffer.payload), t.buffer.retainedBytes, true)
					t.buffer = generatedOutput{}
				}
				return n, true
			}
		}

		if len(t.buffer.payload) == 0 {
			// A stale remainder was dropped at a record boundary. Continue so
			// current-generation queued output can use this Read call.
		} else {
			if traceEnabled {
				t.log().Trace(user, "baseHandler.Read", "using committed output remainder",
					"generation", t.buffer.generation, "bufferedLen", len(t.buffer.payload))
			}
			n = copy(p, t.buffer.payload)
			t.buffer.payload = t.buffer.payload[n:]
			retainedBytes := 0
			entryFinished := len(t.buffer.payload) == 0
			if entryFinished {
				retainedBytes = t.buffer.retainedBytes
			}
			t.releaseOutputLocked(n, retainedBytes, entryFinished)
			if len(t.buffer.payload) == 0 {
				t.buffer = generatedOutput{}
			} else if n > 0 {
				t.buffer.mustFinishRecord = p[n-1] != protocol.MessageDelimiter
			}
			if traceEnabled {
				t.log().Trace(user, "baseHandler.Read", "after buffer read", "copied", n, "remaining", len(t.buffer.payload))
			}
			return n, true
		}
	}

	if !t.mode {
		return 0, false
	}

	if traceEnabled {
		t.log().Trace(user, "baseHandler.Read", "checking output buffer", "bufferedBytes", t.bufferedBytes)
	}

	for len(t.queue) > 0 {
		outputData := t.dequeueLocked()
		if n, delivered := t.consumeLocked(p, outputData, user, traceEnabled, shouldDropGeneration); delivered {
			return n, true
		}
	}

	t.maybeAckEOFLocked(user)

	if traceEnabled {
		t.log().Trace(user, "baseHandler.Read", "no data in output buffer, falling through")
	}
	return 0, false
}

// dequeueLocked removes the oldest descriptor and periodically compacts the
// backing slice. Entries move to the partial buffer without releasing their
// metadata budget until delivery or a generation drop completes.
func (t *outputManager) dequeueLocked() generatedOutput {
	outputData := t.queue[0]
	t.queue[0] = generatedOutput{}
	t.queue = t.queue[1:]
	if len(t.queue) == 0 {
		t.queue = nil
	} else if len(t.queue)*4 <= cap(t.queue) {
		compacted := make([]generatedOutput, len(t.queue))
		copy(compacted, t.queue)
		t.queue = compacted
	}
	return outputData
}

// consumeLocked drops stale-generation output and otherwise copies it into p,
// buffering any remainder for the next read. Returns delivered=false when the
// payload was dropped. The caller must hold t.mu.
func (t *outputManager) consumeLocked(p []byte, outputData generatedOutput, user *user.User,
	traceEnabled bool, shouldDropGeneration func(uint64) bool) (n int, delivered bool) {

	if shouldDropGeneration != nil && shouldDropGeneration(outputData.generation) {
		t.eofEmptySince = time.Time{}
		t.releaseOutputLocked(len(outputData.payload), outputData.retainedBytes, true)
		return 0, false
	}

	if traceEnabled {
		t.log().Trace(user, "baseHandler.Read", "got data from output buffer", "dataLen", len(outputData.payload))
	}
	t.eofEmptySince = time.Time{}
	n = copy(p, outputData.payload)
	entryFinished := n == len(outputData.payload)
	retainedBytes := 0
	if entryFinished {
		retainedBytes = outputData.retainedBytes
	}
	t.releaseOutputLocked(n, retainedBytes, entryFinished)
	if n < len(outputData.payload) {
		t.buffer = generatedOutput{
			generation:       outputData.generation,
			payload:          outputData.payload[n:],
			mustFinishRecord: n > 0 && p[n-1] != protocol.MessageDelimiter,
			retainedBytes:    outputData.retainedBytes,
		}
		if traceEnabled {
			t.log().Trace(user, "baseHandler.Read", "committing remaining data",
				"generation", outputData.generation, "bufferedLen", len(t.buffer.payload))
		}
	}
	return n, true
}

// maybeAckEOFLocked disables output mode and acknowledges the EOF once EOF has
// been signaled and the output buffer has stayed empty for the quiet period.
// The caller must hold t.mu.
func (t *outputManager) maybeAckEOFLocked(user *user.User) {
	if t.eof == nil {
		return
	}

	select {
	case <-t.eof:
	default:
		return
	}

	if t.bufferedBytes > 0 {
		t.eofEmptySince = time.Time{}
		return
	}

	if t.eofEmptySince.IsZero() {
		t.eofEmptySince = time.Now()
		// readWait was snapshotted before tryRead acquired the lock. Wake that
		// waiter so it can snapshot the newly established quiet-period deadline.
		t.signalStateChangedLocked()
		return
	}

	if time.Since(t.eofEmptySince) >= t.resolvedEOFAckQuietPeriod() {
		t.log().Trace(user, "baseHandler.Read", "EOF acknowledged and channel stable-empty, disabling output mode")
		t.mode = false
		t.eofEmptySince = time.Time{}
		t.signalEOFAckLocked()
	}
}
