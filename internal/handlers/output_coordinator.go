package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal"
)

type outputReadKind uint8

const (
	outputReadRetry outputReadKind = iota
	outputReadBytes
	outputReadServerMessage
	outputReadMaprMessage
)

type outputReadResult struct {
	kind    outputReadKind
	n       int
	message string
	err     error
}

type outputCoordinator struct {
	handler *baseHandler
	output  outputManager

	maprMessages   chan string
	serverMessages chan string
	// sessionAcks and ackGate send SESSION acknowledgements ahead of the
	// output of the workload they acknowledge (see sessionAckGate).
	sessionAcks       chan sessionAck
	ackGate           sessionAckGate
	hostname          string
	ackCloseReceived  chan struct{}
	ackCloseOnce      sync.Once
	flushInitOnce     sync.Once
	flushRequests     chan chan struct{}
	flushErrors       chan string
	closeSyncWake     chan struct{}
	pendingFlushAcks  []chan struct{}
	deferredServer    string
	hasDeferredServer bool
	closeSyncObserved bool
	closeSyncEmitted  bool
	outputAbort       *internal.Done
	activeGeneration  func() uint64

	readActive         atomic.Bool
	readerSeen         atomic.Bool
	readBufferedBytes  atomic.Int64
	deliveryPending    atomic.Bool
	closeSyncRequested atomic.Bool
}

var errHandlerFlushTimeout = errors.New("timed out flushing session output")

// AttachOutputReader declares that a transport reader owns session output.
func (c *outputCoordinator) AttachOutputReader() {
	c.readerSeen.Store(true)
}

// EnableDirectOutput enables direct line processing for this session.
func (c *outputCoordinator) EnableDirectOutput() bool {
	return c.output.enable()
}

// DirectOutputActive reports whether direct output is enabled.
func (c *outputCoordinator) DirectOutputActive() bool {
	return c.output.enabled()
}

// HasOutputEOF reports whether the direct-output EOF handshake exists.
func (c *outputCoordinator) HasOutputEOF() bool {
	return c.output.hasEOF()
}

// OutputEpoch returns the current direct-output handshake epoch.
func (c *outputCoordinator) OutputEpoch() uint64 {
	return c.output.currentEpoch()
}

// SignalOutputEOF signals EOF if epoch still identifies the current batch.
func (c *outputCoordinator) SignalOutputEOF(epoch uint64) {
	c.output.signalEOF(epoch)
}

// EnqueueOutput adds payload to the bounded session output buffer.
func (c *outputCoordinator) EnqueueOutput(ctx context.Context, generation uint64, payload []byte,
	activeGeneration func() uint64) error {
	return c.output.enqueue(ctx, generation, payload, activeGeneration)
}

// OutputBufferBytes returns the number of payload bytes awaiting delivery.
func (c *outputCoordinator) OutputBufferBytes() int {
	return c.output.bufferedLen()
}

// WaitForOutputEOFAck waits for reader acknowledgement, cancellation, or timeout.
func (c *outputCoordinator) WaitForOutputEOFAck(ctx context.Context, timeout time.Duration) bool {
	return c.output.waitForEOFAck(ctx, timeout)
}

func (c *outputCoordinator) next(p []byte) outputReadResult {
	h := c.handler
	outputChanged, eofWait := c.output.readWait()
	if result, handled := c.tryReadSessionAckFirst(); handled {
		return result
	}
	if result, handled := c.tryReadPayload(p); handled {
		return result
	}
	if result, handled := c.tryReadQueued(); handled {
		return result
	}

	var eofTimer *time.Timer
	var eofTimerC <-chan time.Time
	if eofWait > 0 {
		eofTimer = time.NewTimer(eofWait)
		eofTimerC = eofTimer.C
	}

	select {
	case message := <-c.flushErrors:
		stopOptionalTimer(eofTimer)
		return outputReadResult{kind: outputReadServerMessage, message: message}
	case ack := <-c.sessionAcks:
		stopOptionalTimer(eofTimer)
		return c.deliverSessionAck(ack)
	case message := <-c.serverMessages:
		stopOptionalTimer(eofTimer)
		return c.takeServerMessage(message)
	case message := <-c.maprSource():
		stopOptionalTimer(eofTimer)
		return c.takeMaprMessage(message)
	case <-h.done.Done():
		stopOptionalTimer(eofTimer)
		if result, handled := c.tryReadPayload(p); handled {
			return result
		}
		if result, handled := c.tryReadQueued(); handled {
			return result
		}
		return outputReadResult{err: io.EOF}
	case flushAck := <-c.flushRequests:
		stopOptionalTimer(eofTimer)
		c.pendingFlushAcks = append(c.pendingFlushAcks, flushAck)
		return outputReadResult{kind: outputReadRetry}
	case <-outputChanged:
		stopOptionalTimer(eofTimer)
		return outputReadResult{kind: outputReadRetry}
	case <-c.closeSyncWake:
		stopOptionalTimer(eofTimer)
		c.closeSyncObserved = true
		return outputReadResult{kind: outputReadRetry}
	case <-eofTimerC:
		return outputReadResult{kind: outputReadRetry}
	}
}

// tryReadPayload reads queued payload into p, if there is any to deliver
// (outputManager.tryRead).
func (c *outputCoordinator) tryReadPayload(p []byte) (outputReadResult, bool) {
	n, handled := c.output.tryRead(p, c.handler.user, c.shouldDropGeneration)
	switch {
	case !handled:
		return outputReadResult{}, false
	case n == 0:
		return outputReadResult{kind: outputReadRetry}, true
	default:
		return outputReadResult{kind: outputReadBytes, n: n}, true
	}
}

// tryReadQueued takes the next queued message without blocking: a SESSION
// acknowledgement (and, once it released its generation, the messages
// deferred for it, see sessionAckGate), a flush error, a server message, a
// MapReduce message; else it handles flush requests and the close handshake.
// next calls it only when no payload is to be read.
func (c *outputCoordinator) tryReadQueued() (outputReadResult, bool) {
	if result, handled := c.tryReadSessionAck(); handled {
		return result, true
	}
	select {
	case message := <-c.flushErrors:
		return outputReadResult{kind: outputReadServerMessage, message: message}, true
	default:
	}
	if c.hasDeferredServer {
		message := c.deferredServer
		c.deferredServer = ""
		c.hasDeferredServer = false
		return outputReadResult{kind: outputReadServerMessage, message: message}, true
	}
	select {
	case message := <-c.serverMessages:
		return c.takeServerMessage(message), true
	default:
	}
	select {
	case message := <-c.maprSource():
		return c.takeMaprMessage(message), true
	default:
	}
	return c.tryFlushOrClose()
}

// tryFlushOrClose collects the pending flush requests and, once nothing is
// queued or held any more, acknowledges them and emits the close handshake
// if one was requested.
func (c *outputCoordinator) tryFlushOrClose() (outputReadResult, bool) {
	receivedFlushRequest := false
	for {
		select {
		case flushAck := <-c.flushRequests:
			c.pendingFlushAcks = append(c.pendingFlushAcks, flushAck)
			receivedFlushRequest = true
		default:
			if receivedFlushRequest {
				return outputReadResult{kind: outputReadRetry}, true
			}
			if c.closeSyncRequested.Load() && !c.closeSyncObserved {
				c.closeSyncObserved = true
				return outputReadResult{kind: outputReadRetry}, true
			}
			if c.ackGate.pending() {
				// Output held for a SESSION acknowledgement is still to
				// be sent: neither flushed nor ready for the close.
				return outputReadResult{}, false
			}
			for _, flushAck := range c.pendingFlushAcks {
				close(flushAck)
			}
			c.pendingFlushAcks = nil
			if c.closeSyncObserved && c.closeSyncRequested.Load() && !c.closeSyncEmitted {
				c.closeSyncEmitted = true
				return outputReadResult{
					kind:    outputReadServerMessage,
					message: ".syn close connection",
				}, true
			}
			return outputReadResult{}, false
		}
	}
}

// tryReadQueuedMapr takes the next queued MapReduce message without
// blocking, for sessionFramer.readMaprMessage to frame it into the same Read
// as the mapr message before it. It takes one only where next would take it
// as well: while no payload, flush error, deferred or queued server message
// or SESSION acknowledgement is pending, all of which next delivers before a
// mapr message, and while no output is held or deferred for an
// acknowledgement (sessionAckGate), which must go first. Flush
// requests and the close handshake come after the queued mapr messages
// anyway (tryReadQueued), so they need no check. A message racing in after
// a check is delivered by the next Read, as it would be without coalescing.
//
// The hasDeferredServer check is defensive and unreachable today: only
// prioritizeFlushError defers a server message, on the output goroutine,
// and the deferred message is taken by the tryReadQueued pass of the next
// Read before any mapr message, so no mapr batch (readMaprMessage) or rest
// of one (drainPending) is framed while a server message is deferred. It
// keeps that ordering should a later change defer messages elsewhere.
func (c *outputCoordinator) tryReadQueuedMapr() (string, bool) {
	if len(c.flushErrors) > 0 || c.hasDeferredServer || len(c.serverMessages) > 0 ||
		len(c.sessionAcks) > 0 || c.ackGate.pending() || c.output.bufferedLen() > 0 {
		return "", false
	}
	select {
	case message := <-c.maprMessages:
		// Checked again once taken: its generation may have been held
		// since (sessionAckGate).
		if c.ackGate.deferIfHeld(outputReadResult{kind: outputReadMaprMessage, message: message}) {
			return "", false
		}
		return message, true
	default:
		return "", false
	}
}

func (c *outputCoordinator) prioritizeFlushError(message string) outputReadResult {
	select {
	case flushError := <-c.flushErrors:
		c.deferredServer = message
		c.hasDeferredServer = true
		return outputReadResult{kind: outputReadServerMessage, message: flushError}
	default:
		return outputReadResult{kind: outputReadServerMessage, message: message}
	}
}

func (c *outputCoordinator) acknowledgeClose() {
	c.ackCloseOnce.Do(func() {
		close(c.ackCloseReceived)
	})
}

func (c *outputCoordinator) waitForCloseAcknowledgement(ctx context.Context) {
	if ctx == nil {
		panic("handlers: nil close acknowledgement context")
	}
	h := c.handler
	// The timeout is for a client that got the close frame and does not
	// acknowledge it (e.g. one that stopped reading). While the reader is still
	// writing to a slow or paused client (clientSlow), the close frame may
	// not even have reached it: closing then would cut the session short.
	const timeout = 5 * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-c.ackCloseReceived:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			if c.clientSlow() {
				timer.Reset(timeout)
				continue
			}
			h.Logger().Debug(h.user, "Shutdown timeout reached, enforcing shutdown")
			return
		case <-h.done.Done():
			return
		}
	}
}

func (c *outputCoordinator) send(ch chan<- string, message string) {
	select {
	case ch <- message:
	case <-c.handler.done.Done():
	}
}

func (c *outputCoordinator) sendln(ch chan<- string, message string) {
	c.send(ch, message+"\n")
}

func (c *outputCoordinator) shouldDropGeneration(generation uint64) bool {
	if generation == 0 || c.activeGeneration == nil {
		return false
	}
	activeGeneration := c.activeGeneration()
	return activeGeneration != 0 && activeGeneration != generation
}

func (c *outputCoordinator) ensureFlushChannels() {
	c.flushInitOnce.Do(func() {
		if c.flushRequests == nil {
			c.flushRequests = make(chan chan struct{})
		}
		if c.flushErrors == nil {
			c.flushErrors = make(chan string, 2)
		}
		if c.closeSyncWake == nil {
			c.closeSyncWake = make(chan struct{}, 1)
		}
	})
}

// queuedUnits counts the output still queued for the session reader: server
// messages and acknowledgements, MapReduce messages and payload bytes. A
// generation held for its acknowledgement counts as one unit until the
// acknowledgement went out (sessionAckGate).
func (c *outputCoordinator) queuedUnits() int {
	h := c.handler
	serverCount := len(c.serverMessages) + len(c.sessionAcks)
	if c.ackGate.held.Load() != 0 {
		serverCount++
	}
	maprCount := len(c.maprMessages)
	outputBytes := c.output.bufferedLen()
	h.Logger().Trace(h.user, "flush", "server", serverCount, "mapr", maprCount,
		"outputBytes", outputBytes)
	return serverCount + maprCount + outputBytes
}

func (c *outputCoordinator) flushContext(ctx context.Context) error {
	h := c.handler
	if ctx == nil {
		panic("handlers: nil flush context")
	}
	c.ensureFlushChannels()
	h.Logger().Trace(h.user, "flush()")
	maxWait := c.output.resolvedFlushTimeout()
	if !c.readerSeen.Load() {
		return nil
	}
	if c.queuedUnits() == 0 && c.readBufferedBytes.Load() == 0 &&
		!c.readActive.Load() && !c.deliveryPending.Load() {
		h.Logger().Debug(h.user, "ALL lines sent", fmt.Sprintf("%p", h))
		return nil
	}

	// The timeout does not run out while the reader is still delivering to a
	// slow or paused client (see clientSlow and outputManager.flush): the
	// flush then waits for the client, the session's context or its end.
	flushAck := make(chan struct{})
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	requests := c.flushRequests
	for {
		select {
		case requests <- flushAck:
			// Requested; from now on only wait for the acknowledgement.
			requests = nil
		case <-flushAck:
			h.Logger().Debug(h.user, "ALL lines sent", fmt.Sprintf("%p", h))
			return nil
		case <-ctx.Done():
			return fmt.Errorf("flush session output: %w", ctx.Err())
		case <-h.done.Done():
			return fmt.Errorf("flush session output: handler stopped")
		case <-timer.C:
			select {
			case <-flushAck:
				h.Logger().Debug(h.user, "ALL lines sent", fmt.Sprintf("%p", h))
				return nil
			default:
			}
			if c.clientSlow() {
				timer.Reset(maxWait)
				continue
			}
			return fmt.Errorf("%w after %s: %d queued units remain",
				errHandlerFlushTimeout, maxWait, c.queuedUnits())
		}
	}
}

// clientSlow reports whether the session reader is still delivering bytes it
// already took to the client: its last Read returned bytes and the transport
// has not come back for more. The reader is then blocked writing to a client
// that reads slowly or not at all right now, which is no reason to give up on
// the output still queued. It is false once the session ended.
func (c *outputCoordinator) clientSlow() bool {
	select {
	case <-c.handler.done.Done():
		return false
	default:
	}
	return c.deliveryPending.Load()
}

func (c *outputCoordinator) flushOutput(ctx context.Context) error {
	h := c.handler
	if err := c.output.flush(ctx, h.user); err != nil {
		return err
	}
	if !c.readActive.Load() && !c.deliveryPending.Load() {
		return nil
	}
	return c.flushContext(ctx)
}

func (c *outputCoordinator) reportFlushError(generation uint64, err error) {
	if err == nil {
		return
	}
	h := c.handler
	c.ensureFlushChannels()
	h.Logger().Error(h.user, "Unable to flush session output", err)
	message := encodeGeneratedMessage(generation,
		fmt.Sprintf("Unable to flush session output: %v\n", err))
	select {
	case c.flushErrors <- message:
	default:
		h.Logger().Error(h.user, "Unable to queue flush error for client", err)
	}
}

func (c *outputCoordinator) requestCloseSync(ctx context.Context) {
	if ctx == nil {
		panic("handlers: nil close synchronization context")
	}
	h := c.handler
	if !c.readerSeen.Load() {
		go func() {
			defer recoverHandlerPanic(h.Logger(), h.user, "shutdown acknowledgement sender", h.abortAfterPanic)
			select {
			case c.serverMessages <- ".syn close connection":
			case <-ctx.Done():
			case <-h.done.Done():
			}
		}()
		return
	}

	c.ensureFlushChannels()
	c.closeSyncRequested.Store(true)
	select {
	case c.closeSyncWake <- struct{}{}:
	default:
	}
}

func (c *outputCoordinator) outputAbortDone() <-chan struct{} {
	return c.outputAbort.Done()
}
