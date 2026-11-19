package handlers

import "sync/atomic"

// sessionAckQueueSize bounds the SESSION acknowledgements waiting for the
// output goroutine. The commands of a session are handled one after another
// on its input goroutine, so one is the usual most.
const sessionAckQueueSize = 4

// sessionAck is a SESSION START/UPDATE acknowledgement (ok or error) queued
// for the output goroutine. generation is the server's number of the
// workload whose output waits for it (sessionAckGate.hold), 0 for an
// acknowledgement that releases nothing (e.g. a request rejected before a
// workload started).
type sessionAck struct {
	message    string
	generation uint64
}

// sessionAckGate makes the acknowledgement of a SESSION START/UPDATE reach
// the client ahead of every output of the workload it acknowledges.
//
// The session output delivers payload before server messages. An
// acknowledgement queued as a server message after the read commands were
// dispatched therefore waited behind the whole session output buffer, and
// on a slow link the client's 2 s acknowledgement wait ran out.
// Now:
//
//   - start and update hold the new generation (hold) before they dispatch
//     its commands, so before any output of it exists;
//   - output of a held generation is not delivered: payload stays queued
//     (outputManager.tryRead stops at its first entry), and server and
//     MapReduce messages and flush errors (takeFlushError) the output
//     goroutine takes are deferred, in order;
//   - acknowledgements go to a queue of their own (sessionAcks), which the
//     output goroutine reads before payload, as soon as that splits no
//     frame (tryReadSessionAckFirst), and before the other server messages;
//     taking the acknowledgement of the held generation releases it
//     (release), so its output can not overtake it whatever the timing;
//   - a workload that fails to start is released by the input goroutine
//     itself (sessionCommandState.dispatch, releaseSessionOutput), as its
//     error acknowledgement releases nothing;
//   - the deferred messages then go out before any new message.
//
// Acknowledgements that release nothing (errors) go ahead of payload too: a
// rejected SESSION UPDATE leaves the running generation's payload
// deliverable, which can fill the whole session output buffer. Output of
// earlier generations is not held: a frame already begun is finished before
// the acknowledgement, and the rest of a replaced generation is dropped as
// stale (shouldDropGeneration). A non-SESSION command stream holds nothing
// and sends no acknowledgement, so its order is unchanged, and so is the
// order among payload and server messages of a running workload. A failure
// report of the new workload goes out after its acknowledgement.
//
// The deferred messages are not capped, as refusing to take them could
// block the input goroutine before it queued the acknowledgement that ends
// the hold: the dispatch itself may send server messages synchronously
// (e.g. a rejected command's error). Their number is bounded by what the
// workload's commands send before the acknowledgement went out, about three
// messages per read command of the spec (e.g. a missing file's warnings and
// failure report); a hold lasts only while the commands are dispatched and
// the acknowledgement is queued, so they are transient (about 3.5 MB were
// measured for a spec of 20,000 missing files).
//
// held is written by the input goroutine (hold, and release on a failed
// start) and the output goroutine (release on taking the acknowledgement)
// and read by both; deferred and maprDeferred belong to the output
// goroutine alone.
type sessionAckGate struct {
	held         atomic.Uint64
	deferred     []outputReadResult
	maprDeferred bool
}

// hold holds back the output of generation until its acknowledgement was
// taken by the output goroutine. A newer hold replaces an older one, whose
// output is stale by then.
func (g *sessionAckGate) hold(generation uint64) {
	g.held.Store(generation)
}

// release ends the hold of generation, if it is still the held one.
func (g *sessionAckGate) release(generation uint64) {
	if generation != 0 {
		g.held.CompareAndSwap(generation, 0)
	}
}

// holds reports whether the output of generation is held back.
func (g *sessionAckGate) holds(generation uint64) bool {
	return generation != 0 && generation == g.held.Load()
}

// holdsMessage reports whether message (a generation-tagged server or
// MapReduce message) belongs to the held generation. It decodes the tag
// only while a generation is held.
func (g *sessionAckGate) holdsMessage(message string) bool {
	if g.held.Load() == 0 {
		return false
	}
	generation, _ := decodeGeneratedMessage(message)
	return g.holds(generation)
}

// pending reports whether output is held back or deferred, i.e. whether
// the session output can not yet be called flushed.
func (g *sessionAckGate) pending() bool {
	return g.held.Load() != 0 || len(g.deferred) > 0
}

// deferIfHeld defers result, a message just taken, when it belongs to the
// held generation or when earlier messages are still deferred (it must not
// overtake them). It reports whether it deferred it.
func (g *sessionAckGate) deferIfHeld(result outputReadResult) bool {
	if len(g.deferred) == 0 && !g.holdsMessage(result.message) {
		return false
	}
	g.deferred = append(g.deferred, result)
	if result.kind == outputReadMaprMessage {
		g.maprDeferred = true
	}
	return true
}

// takeDeferred returns the oldest deferred message once no generation is
// held any more.
func (g *sessionAckGate) takeDeferred() (outputReadResult, bool) {
	if len(g.deferred) == 0 || g.held.Load() != 0 {
		return outputReadResult{}, false
	}
	result := g.deferred[0]
	g.deferred[0] = outputReadResult{}
	g.deferred = g.deferred[1:]
	if len(g.deferred) == 0 {
		g.deferred = nil
		g.maprDeferred = false
	}
	return result, true
}

// holdSessionOutput holds back the output of generation until its
// acknowledgement goes out (see sessionAckGate).
func (c *outputCoordinator) holdSessionOutput(generation uint64) {
	c.ackGate.hold(generation)
}

// releaseSessionOutput ends the hold of generation without an
// acknowledgement, for a workload that failed to start.
func (c *outputCoordinator) releaseSessionOutput(generation uint64) {
	c.ackGate.release(generation)
}

// sendSessionAck queues a SESSION acknowledgement for the output goroutine.
func (c *outputCoordinator) sendSessionAck(ack sessionAck) {
	select {
	case c.sessionAcks <- ack:
	case <-c.handler.done.Done():
	}
}

// deliverSessionAck turns an acknowledgement the output goroutine took into
// its server message and releases the generation it acknowledges.
func (c *outputCoordinator) deliverSessionAck(ack sessionAck) outputReadResult {
	c.ackGate.release(ack.generation)
	return outputReadResult{kind: outputReadServerMessage, message: ack.message}
}

// tryReadSessionAckFirst takes a queued acknowledgement ahead of any
// payload, as long as that splits no frame (outputManager.atRecordBoundary):
// the payload still deliverable, of a generation the acknowledgement does
// not replace, can be the whole session output buffer. An error
// acknowledgement of a rejected SESSION UPDATE leaves the running
// generation's payload deliverable, and the client waits only 2 s for it.
// The queue length is checked first, so the hot path takes no lock.
func (c *outputCoordinator) tryReadSessionAckFirst() (outputReadResult, bool) {
	if len(c.sessionAcks) == 0 || !c.output.atRecordBoundary() {
		return outputReadResult{}, false
	}
	select {
	case ack := <-c.sessionAcks:
		return c.deliverSessionAck(ack), true
	default:
		return outputReadResult{}, false
	}
}

// tryReadSessionAck takes a queued acknowledgement or, once no generation is
// held, a deferred message, without blocking.
func (c *outputCoordinator) tryReadSessionAck() (outputReadResult, bool) {
	select {
	case ack := <-c.sessionAcks:
		return c.deliverSessionAck(ack), true
	default:
	}
	return c.ackGate.takeDeferred()
}

// takeServerMessage returns a server message the output goroutine took, or
// a retry when it was deferred (sessionAckGate.deferIfHeld).
func (c *outputCoordinator) takeServerMessage(message string) outputReadResult {
	if c.ackGate.deferIfHeld(outputReadResult{kind: outputReadServerMessage, message: message}) {
		return outputReadResult{kind: outputReadRetry}
	}
	return c.prioritizeFlushError(message)
}

// takeMaprMessage returns a MapReduce message the output goroutine took, or
// a retry when it was deferred.
func (c *outputCoordinator) takeMaprMessage(message string) outputReadResult {
	result := outputReadResult{kind: outputReadMaprMessage, message: message}
	if c.ackGate.deferIfHeld(result) {
		return outputReadResult{kind: outputReadRetry}
	}
	return result
}

// maprSource is the MapReduce queue while its messages may be taken: not
// while one of them is deferred, as those behind it must wait for it.
func (c *outputCoordinator) maprSource() <-chan string {
	if c.ackGate.maprDeferred {
		return nil
	}
	return c.maprMessages
}
