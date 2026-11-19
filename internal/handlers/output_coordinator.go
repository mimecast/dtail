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

	maprMessages      chan string
	serverMessages    chan string
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
	if n, handled := c.output.tryRead(p, h.user, c.shouldDropGeneration); handled {
		if n == 0 {
			return outputReadResult{kind: outputReadRetry}
		}
		return outputReadResult{kind: outputReadBytes, n: n}
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
	case message := <-c.serverMessages:
		stopOptionalTimer(eofTimer)
		return c.prioritizeFlushError(message)
	case message := <-c.maprMessages:
		stopOptionalTimer(eofTimer)
		return outputReadResult{kind: outputReadMaprMessage, message: message}
	case <-h.done.Done():
		stopOptionalTimer(eofTimer)
		if n, handled := c.output.tryRead(p, h.user, c.shouldDropGeneration); handled {
			if n == 0 {
				return outputReadResult{kind: outputReadRetry}
			}
			return outputReadResult{kind: outputReadBytes, n: n}
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

func (c *outputCoordinator) tryReadQueued() (outputReadResult, bool) {
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
		return c.prioritizeFlushError(message), true
	default:
	}
	select {
	case message := <-c.maprMessages:
		return outputReadResult{kind: outputReadMaprMessage, message: message}, true
	default:
	}

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
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-c.ackCloseReceived:
	case <-ctx.Done():
	case <-timer.C:
		h.Logger().Debug(h.user, "Shutdown timeout reached, enforcing shutdown")
	case <-h.done.Done():
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

func (c *outputCoordinator) flushContext(ctx context.Context) error {
	h := c.handler
	if ctx == nil {
		panic("handlers: nil flush context")
	}
	c.ensureFlushChannels()
	h.Logger().Trace(h.user, "flush()")
	queuedUnits := func() int {
		serverCount := len(c.serverMessages)
		maprCount := len(c.maprMessages)
		outputBytes := c.output.bufferedLen()
		h.Logger().Trace(h.user, "flush", "server", serverCount, "mapr", maprCount,
			"outputBytes", outputBytes)
		return serverCount + maprCount + outputBytes
	}

	maxWait := c.output.resolvedFlushTimeout()
	if !c.readerSeen.Load() {
		return nil
	}
	if queuedUnits() == 0 && c.readBufferedBytes.Load() == 0 &&
		!c.readActive.Load() && !c.deliveryPending.Load() {
		h.Logger().Debug(h.user, "ALL lines sent", fmt.Sprintf("%p", h))
		return nil
	}

	flushAck := make(chan struct{})
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case c.flushRequests <- flushAck:
	case <-ctx.Done():
		return fmt.Errorf("flush session output: %w", ctx.Err())
	case <-h.done.Done():
		return fmt.Errorf("flush session output: handler stopped")
	case <-timer.C:
		return fmt.Errorf("%w after %s: %d queued units remain",
			errHandlerFlushTimeout, maxWait, queuedUnits())
	}

	select {
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
		return fmt.Errorf("%w after %s: %d queued units remain",
			errHandlerFlushTimeout, maxWait, queuedUnits())
	}
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
