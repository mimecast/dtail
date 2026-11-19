package handlers

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// readSessionAck takes the next SESSION acknowledgement the way the output
// goroutine does (deliverSessionAck, which releases its generation) and
// returns its message.
func readSessionAck(t *testing.T, handler *ServerHandler) string {
	t.Helper()
	select {
	case ack := <-handler.sessionAcks:
		return handler.deliverSessionAck(ack).message
	case <-time.After(time.Second):
		t.Fatalf("Timed out waiting for a session acknowledgement")
		return ""
	}
}

// drainQueuedMessages takes every message the output goroutine would take
// without blocking (tryReadQueued) and returns them in order.
func drainQueuedMessages(c *outputCoordinator) []string {
	var messages []string
	for {
		result, handled := c.tryReadQueued()
		if !handled {
			return messages
		}
		if result.kind == outputReadServerMessage || result.kind == outputReadMaprMessage {
			messages = append(messages, result.message)
		}
	}
}

func requireMessages(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("messages = %q, want %q", got, want)
		}
	}
}

// TestSessionAckGateDefersHeldGenerationMessages: server and MapReduce
// messages of a held generation, and every message behind them, wait for
// its acknowledgement, which goes first, and keep their order; messages of
// other generations before them are not held, and no flush is acknowledged
// while output is held.
func TestSessionAckGateDefersHeldGenerationMessages(t *testing.T) {
	handler := newSessionTestHandler("ack-gate-user")
	c := handler.outputCoordinator
	c.ensureFlushChannels()
	flushAck := make(chan struct{})
	c.pendingFlushAcks = append(c.pendingFlushAcks, flushAck)

	c.holdSessionOutput(3)
	handler.serverMessages <- encodeGeneratedMessage(2, "earlier warning")
	handler.serverMessages <- encodeGeneratedMessage(3, "held warning")
	handler.serverMessages <- "unrelated message"
	handler.maprMessages <- encodeGeneratedMessage(3, "held aggregate")
	handler.maprMessages <- encodeGeneratedMessage(3, "next aggregate")

	before := drainQueuedMessages(c)
	requireMessages(t, before, protocol.HiddenCapabilitiesPrefix+protocol.CapabilityJournalV1, encodeGeneratedMessage(2, "earlier warning"))
	if len(handler.maprMessages) != 1 {
		t.Fatalf("mapr messages taken behind a deferred one: %d queued, want 1", len(handler.maprMessages))
	}
	select {
	case <-flushAck:
		t.Fatal("flush acknowledged while output was held for a session acknowledgement")
	default:
	}

	c.sendSessionAck(sessionAck{message: sessionAckStartOKPrefix + " 1", generation: 3})
	after := drainQueuedMessages(c)
	requireMessages(t, after, sessionAckStartOKPrefix+" 1",
		encodeGeneratedMessage(3, "held warning"), "unrelated message",
		encodeGeneratedMessage(3, "held aggregate"), encodeGeneratedMessage(3, "next aggregate"))
	if c.ackGate.pending() {
		t.Fatal("output still held after the acknowledgement")
	}
	select {
	case <-flushAck:
	default:
		t.Fatal("flush not acknowledged once the held output went out")
	}
}

// TestSessionAckGateDefersHeldGenerationFlushErrors: the flush error of a
// read batch of a held generation waits for that generation's
// acknowledgement on each of the three ways the output goroutine takes a
// flush error (tryReadQueued, ahead of a server message, waitForOutput),
// while a flush error of no generation or of another one is not held.
func TestSessionAckGateDefersHeldGenerationFlushErrors(t *testing.T) {
	flushError := func(generation uint64) string {
		return encodeGeneratedMessage(generation, "Unable to flush session output: stalled\n")
	}
	ack := sessionAck{message: sessionAckUpdateOKPrefix + " 2", generation: 3}

	t.Run("queued", func(t *testing.T) {
		handler := newSessionTestHandler("ack-gate-flush-user")
		c := handler.outputCoordinator
		drainQueuedMessages(c)
		c.holdSessionOutput(3)
		c.reportFlushError(0, errors.New("stalled"))
		c.reportFlushError(2, errors.New("stalled"))
		requireMessages(t, drainQueuedMessages(c), flushError(0), flushError(2))
		c.reportFlushError(3, errors.New("stalled"))
		requireMessages(t, drainQueuedMessages(c))
		if !c.ackGate.pending() {
			t.Fatal("no output pending although a flush error is deferred")
		}
		c.sendSessionAck(ack)
		requireMessages(t, drainQueuedMessages(c), ack.message, flushError(3))
	})

	t.Run("ahead of a server message", func(t *testing.T) {
		handler := newSessionTestHandler("ack-gate-flush-prio-user")
		c := handler.outputCoordinator
		drainQueuedMessages(c)
		c.holdSessionOutput(3)
		c.reportFlushError(3, errors.New("stalled"))
		result := c.takeServerMessage("running workload's warning")
		if result.kind != outputReadServerMessage || result.message != "running workload's warning" {
			t.Fatalf("server message = %+v, want it delivered, the held flush error overtakes nothing", result)
		}
		if c.hasDeferredServer {
			t.Fatal("server message kept back for a flush error that was deferred")
		}
		c.sendSessionAck(ack)
		requireMessages(t, drainQueuedMessages(c), ack.message, flushError(3))
	})

	t.Run("while waiting", func(t *testing.T) {
		handler := newSessionTestHandler("ack-gate-flush-wait-user")
		c := handler.outputCoordinator
		drainQueuedMessages(c)
		c.holdSessionOutput(3)
		c.reportFlushError(3, errors.New("stalled"))
		if result := c.waitForOutput(nil, nil, 0); result.kind != outputReadRetry {
			t.Fatalf("waitForOutput = %+v, want a retry with the flush error deferred", result)
		}
		c.sendSessionAck(ack)
		requireMessages(t, drainQueuedMessages(c), ack.message, flushError(3))
	})

	t.Run("not held: a flush error still goes before a server message", func(t *testing.T) {
		handler := newSessionTestHandler("ack-gate-flush-free-user")
		c := handler.outputCoordinator
		drainQueuedMessages(c)
		c.reportFlushError(3, errors.New("stalled"))
		if result := c.takeServerMessage("warning"); result.message != flushError(3) {
			t.Fatalf("takeServerMessage = %+v, want the flush error first", result)
		}
		requireMessages(t, drainQueuedMessages(c), "warning")
	})
}

// TestSessionAckGateKeepsHoldForOtherAcks: an acknowledgement of no or of
// another generation releases nothing (negative cases), and a newer hold
// replaces an older one.
func TestSessionAckGateKeepsHoldForOtherAcks(t *testing.T) {
	handler := newSessionTestHandler("ack-gate-other-user")
	c := handler.outputCoordinator
	drainQueuedMessages(c)

	c.holdSessionOutput(5)
	c.sendSessionAck(sessionAck{message: sessionAckErrorPrefix + "invalid session payload"})
	c.sendSessionAck(sessionAck{message: sessionAckStartOKPrefix + " 1", generation: 4})
	handler.serverMessages <- encodeGeneratedMessage(5, "held warning")
	got := drainQueuedMessages(c)
	requireMessages(t, got, sessionAckErrorPrefix+"invalid session payload", sessionAckStartOKPrefix+" 1")
	if !c.ackGate.holds(5) {
		t.Fatal("generation 5 released by an acknowledgement of another generation")
	}

	c.holdSessionOutput(6)
	if c.ackGate.holds(5) || !c.ackGate.holds(6) {
		t.Fatal("a newer hold did not replace the older one")
	}
	c.releaseSessionOutput(6)
	requireMessages(t, drainQueuedMessages(c), encodeGeneratedMessage(5, "held warning"))
}

// TestOutputManagerStopsAtHeldGeneration: tryRead delivers queued payload of
// other generations up to the first entry of the held generation, and that
// entry once the hold ended.
func TestOutputManagerStopsAtHeldGeneration(t *testing.T) {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{readRetryInterval: time.Millisecond}, handlerTestLogger)
	var gate sessionAckGate
	manager.heldGeneration = gate.holds
	manager.enable()
	gate.hold(2)
	mustEnqueueOutput(t, manager, 1, []byte("old"))
	mustEnqueueOutput(t, manager, 2, []byte("new"))
	user := &userserver.User{Name: "held-generation-test"}
	buf := make([]byte, 16)

	if n, handled := manager.tryRead(buf, user, nil); !handled || string(buf[:n]) != "old" {
		t.Fatalf("first read = handled %v, payload %q; want old", handled, buf[:n])
	}
	if n, handled := manager.tryRead(buf, user, nil); handled || n != 0 {
		t.Fatalf("read of the held generation = handled %v, payload %q; want nothing", handled, buf[:n])
	}
	gate.release(2)
	if n, handled := manager.tryRead(buf, user, nil); !handled || string(buf[:n]) != "new" {
		t.Fatalf("read after the release = handled %v, payload %q; want new", handled, buf[:n])
	}
}

// TestCoalescedAggregatesWaitForSessionAck: a MapReduce message of another
// generation is framed at once, but the held generation's aggregates queued
// behind it are not coalesced into the same Read
// (sessionFramer.readMaprMessage, outputCoordinator.tryReadQueuedMapr):
// they go out after the acknowledgement.
func TestCoalescedAggregatesWaitForSessionAck(t *testing.T) {
	handler := newSessionTestHandler("ack-gate-mapr-user")
	p := make([]byte, 64*1024)
	if n, err := handler.Read(p); err != nil || !strings.Contains(string(p[:n]), protocol.HiddenCapabilitiesPrefix) {
		t.Fatalf("first Read = %q, %v; want the capabilities frame", p[:n], err)
	}

	handler.holdSessionOutput(3)
	handler.maprMessages <- encodeGeneratedMessage(0, "first aggregate")
	handler.maprMessages <- encodeGeneratedMessage(3, "held aggregate")
	n, err := handler.Read(p)
	if err != nil || !strings.Contains(string(p[:n]), "first aggregate") ||
		strings.Contains(string(p[:n]), "held aggregate") {
		t.Fatalf("Read = %q, %v; want the first aggregate alone", p[:n], err)
	}

	handler.sendSessionAck(sessionAck{message: sessionAckStartOKPrefix + " 1", generation: 3})
	var rest strings.Builder
	for !strings.Contains(rest.String(), "held aggregate") {
		n, err := handler.Read(p)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		rest.Write(p[:n])
	}
	requireBefore(t, rest.String(), sessionAckStartOKPrefix+" 1", "held aggregate")
}

// TestQueuedSessionAckStopsAggregateCoalescing: an acknowledgement queued
// while aggregates are coalesced ends the coalescing, so that the next Read
// sends it before them, also one that releases nothing.
func TestQueuedSessionAckStopsAggregateCoalescing(t *testing.T) {
	handler := newSessionTestHandler("ack-gate-coalesce-user")
	c := handler.outputCoordinator
	c.ensureFlushChannels()
	readServerMessage(t, handler.serverMessages)
	handler.maprMessages <- "aggregate"
	c.sendSessionAck(sessionAck{message: sessionAckErrorPrefix + "invalid session payload"})
	if message, taken := c.tryReadQueuedMapr(); taken {
		t.Fatalf("coalesced %q ahead of a queued acknowledgement", message)
	}
}
