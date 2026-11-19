package handlers

// Race and behavior tests for outputManager: command goroutines (enable,
// signalEOF, flush, bufferedLen, waitForEOFAck) run concurrently with the
// session output goroutine (baseHandler.Read -> tryRead). Before outputManager
// state was guarded by a mutex this was a data race on mode, the channel
// fields, buffer and eofEmptySince. Run with `go test -race` to exercise it.
//
// Stale-generation dropping in tryRead is covered separately by
// TestOutputManagerTryReadDropsStaleGeneration in generation_output_test.go.

import (
	"context"
	"sync"
	"testing"
	"time"

	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// handshakeChannels snapshots the EOF handshake channels under the manager's
// lock so tests never bypass the locking discipline.
func handshakeChannels(manager *outputManager) (eof, eofAck chan struct{}) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.eof, manager.eofAck
}

// eofClosed reports whether the given EOF handshake channel is closed.
func eofClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// newHandshakeTestManager returns a manager with tiny intervals so handshake
// tests churn through quiet periods quickly.
func newHandshakeTestManager() *outputManager {
	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		readRetryInterval: time.Microsecond,
		eofAckQuietPeriod: time.Millisecond,
	}, handlerTestLogger)
	return manager
}

func mustEnqueueOutput(t *testing.T, manager *outputManager, generation uint64, payload []byte) {
	t.Helper()
	if err := manager.enqueue(context.Background(), generation, payload, nil); err != nil {
		t.Fatalf("enqueue output: %v", err)
	}
}

// driveReaderUntilDisabled pumps tryRead (as the session output goroutine
// would) until the manager acknowledges the EOF and disables output mode.
func driveReaderUntilDisabled(t *testing.T, manager *outputManager, testUser *userserver.User) {
	t.Helper()
	buf := make([]byte, 64)
	deadline := time.Now().Add(5 * time.Second)
	for manager.enabled() {
		if time.Now().After(deadline) {
			t.Fatal("output mode was not disabled within the deadline")
		}
		manager.tryRead(buf, testUser, nil)
	}
}

// TestOutputManagerConcurrentEnableAndTryRead hammers outputManager from a
// simulated command goroutine and a simulated session reader goroutine at the
// same time. The race detector fails this test when outputManager state is
// accessed without synchronization.
func TestOutputManagerConcurrentEnableAndTryRead(t *testing.T) {

	manager := &outputManager{}
	manager.configure(outputManagerConfig{
		// Keep retry/quiet intervals tiny so the test churns through many
		// mode transitions quickly.
		readRetryInterval: time.Microsecond,
		eofAckQuietPeriod: time.Microsecond,
	}, handlerTestLogger)

	testUser := &userserver.User{Name: "output-race-test"}
	done := make(chan struct{})
	var wg sync.WaitGroup

	// Command goroutine: enable output mode, write a payload, signal EOF and
	// wait for the reader's acknowledgement, over and over.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			manager.enable()
			if err := manager.enqueue(context.Background(), 0, []byte("payload"), nil); err != nil {
				t.Errorf("enqueue output: %v", err)
				return
			}
			_ = manager.flush(context.Background(), testUser)
			manager.signalEOF(manager.currentEpoch())
			manager.waitForEOFAck(context.Background(), 50*time.Millisecond)
			manager.bufferedLen()
			manager.hasEOF()
		}
		close(done)
	}()

	// Session reader goroutine: mirrors baseHandler.Read draining output data.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		for {
			select {
			case <-done:
				return
			default:
			}
			manager.tryRead(buf, testUser, nil)
			manager.enabled()
		}
	}()

	wg.Wait()
}

// TestOutputManagerEnableIsIdempotentWhileEnabled verifies the atomic
// check-and-enable semantics: a second enable while output mode is active (and
// the EOF handshake still live) must report false and must not replace the
// EOF handshake channels of the in-flight batch.
func TestOutputManagerEnableIsIdempotentWhileEnabled(t *testing.T) {
	manager := &outputManager{}

	if !manager.enable() {
		t.Fatal("first enable must report the off->on transition")
	}
	firstEOF, firstEOFAck := handshakeChannels(manager)

	if manager.enable() {
		t.Fatal("second enable must report that output mode was already on")
	}
	secondEOF, secondEOFAck := handshakeChannels(manager)
	if secondEOF != firstEOF || secondEOFAck != firstEOFAck {
		t.Fatal("enable while enabled must not replace a live EOF handshake")
	}
	if !manager.enabled() {
		t.Fatal("output mode must remain enabled")
	}
}

// TestOutputManagerEOFHandshakeEndToEnd runs the full handshake: a payload is
// delivered, EOF is signaled, the reader observes the quiet period, disables
// output mode and acknowledges, and waitForEOFAck reports success.
func TestOutputManagerEOFHandshakeEndToEnd(t *testing.T) {

	manager := newHandshakeTestManager()
	testUser := &userserver.User{Name: "output-handshake-test"}

	if !manager.enable() {
		t.Fatal("enable must report the off->on transition")
	}
	mustEnqueueOutput(t, manager, 0, []byte("payload"))

	buf := make([]byte, 64)
	n, handled := manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "payload" {
		t.Fatalf("expected payload delivery, got handled=%v data=%q", handled, buf[:n])
	}

	manager.signalEOF(manager.currentEpoch())
	driveReaderUntilDisabled(t, manager, testUser)

	if !manager.waitForEOFAck(context.Background(), time.Second) {
		t.Fatal("waitForEOFAck must succeed after the reader acknowledged")
	}
}

// TestOutputManagerReenableAfterAckMintsFreshHandshake verifies the off->on
// half of the handshake invariant: after a completed EOF handshake disabled
// output mode, the next enable must report the transition and create fresh,
// unclosed handshake channels.
func TestOutputManagerReenableAfterAckMintsFreshHandshake(t *testing.T) {

	manager := newHandshakeTestManager()
	testUser := &userserver.User{Name: "output-reenable-test"}

	manager.enable()
	firstEOF, firstEOFAck := handshakeChannels(manager)
	manager.signalEOF(manager.currentEpoch())
	driveReaderUntilDisabled(t, manager, testUser)

	if !manager.enable() {
		t.Fatal("re-enable after a completed handshake must report the off->on transition")
	}
	secondEOF, secondEOFAck := handshakeChannels(manager)
	if secondEOF == firstEOF || secondEOFAck == firstEOFAck {
		t.Fatal("re-enable must mint fresh EOF handshake channels")
	}
	if eofClosed(secondEOF) {
		t.Fatal("fresh EOF channel must not be closed")
	}
}

// TestOutputManagerEnableRefreshesUnackedEOFHandshake is the regression test
// for the stale-handshake bug: batch A signals EOF but the reader never
// acknowledges it (e.g. WaitForOutputEOFAck timed out), so output mode is still
// on with a closed EOF channel. A new batch's enable must refresh the
// handshake, otherwise the new batch would inherit the closed channel and be
// disabled mid-batch after a brief channel-empty period, stranding its output.
func TestOutputManagerEnableRefreshesUnackedEOFHandshake(t *testing.T) {

	manager := newHandshakeTestManager()
	testUser := &userserver.User{Name: "output-stale-eof-test"}

	// Batch A: EOF signaled, but the reader never runs, so no ack happens.
	manager.enable()
	staleEOF, staleEOFAck := handshakeChannels(manager)
	manager.signalEOF(manager.currentEpoch())
	if !manager.enabled() {
		t.Fatal("output mode must still be on while the EOF is unacknowledged")
	}

	// Batch B: enable on the kept-alive session must refresh the handshake.
	if manager.enable() {
		t.Fatal("enable must still report that output mode was already on")
	}
	freshEOF, freshEOFAck := handshakeChannels(manager)
	if freshEOF == staleEOF || freshEOFAck == staleEOFAck {
		t.Fatal("enable must mint fresh handshake channels when the old EOF is closed but unacknowledged")
	}
	if eofClosed(freshEOF) {
		t.Fatal("refreshed EOF channel must not be closed")
	}

	// Batch B must survive channel-empty reads well past the quiet period.
	buf := make([]byte, 64)
	deadline := time.Now().Add(20 * time.Millisecond)
	for time.Now().Before(deadline) {
		manager.tryRead(buf, testUser, nil)
	}
	if !manager.enabled() {
		t.Fatal("output mode must not be disabled by the previous batch's stale EOF")
	}

	// Batch B's own handshake still completes normally.
	mustEnqueueOutput(t, manager, 0, []byte("batch-b"))
	n, handled := manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "batch-b" {
		t.Fatalf("expected batch B payload, got handled=%v data=%q", handled, buf[:n])
	}
	manager.signalEOF(manager.currentEpoch())
	driveReaderUntilDisabled(t, manager, testUser)
	if !manager.waitForEOFAck(context.Background(), time.Second) {
		t.Fatal("batch B's EOF handshake must complete")
	}
}

// TestOutputManagerStaleEpochSignalEOFIsDropped is the regression test for the
// pending-check/signal interleaving: batch A observes pending==0 and captures
// the handshake epoch, batch B joins (enable) while A is still flushing (the
// EOF channel is not yet closed, so no stale-handshake refresh happens), and
// only then does A signal EOF. A's signal carries a stale epoch and must be
// dropped — otherwise B would inherit a closed EOF channel and be disabled
// mid-batch at the first channel-empty quiet period, stranding its output.
func TestOutputManagerStaleEpochSignalEOFIsDropped(t *testing.T) {

	manager := newHandshakeTestManager()
	testUser := &userserver.User{Name: "output-stale-epoch-test"}

	// Batch A enables and — having observed no pending work — captures the
	// epoch it will later signal with.
	manager.enable()
	staleEpoch := manager.currentEpoch()

	// Batch B joins the live session before A gets around to signaling
	// (in production A is stuck in FlushOutput here).
	if manager.enable() {
		t.Fatal("join while enabled must report that output mode was already on")
	}

	// A's stale signal must not close the shared EOF channel.
	manager.signalEOF(staleEpoch)
	eof, _ := handshakeChannels(manager)
	if eofClosed(eof) {
		t.Fatal("a stale-epoch signalEOF must not close the EOF channel")
	}

	// B must survive channel-empty reads well past the quiet period.
	buf := make([]byte, 64)
	deadline := time.Now().Add(20 * time.Millisecond)
	for time.Now().Before(deadline) {
		manager.tryRead(buf, testUser, nil)
	}
	if !manager.enabled() {
		t.Fatal("output mode must not be disabled by a stale-epoch EOF signal")
	}

	// B's own current-epoch signal completes the handshake normally.
	mustEnqueueOutput(t, manager, 0, []byte("batch-b"))
	n, handled := manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "batch-b" {
		t.Fatalf("expected batch B payload, got handled=%v data=%q", handled, buf[:n])
	}
	manager.signalEOF(manager.currentEpoch())
	driveReaderUntilDisabled(t, manager, testUser)
	if !manager.waitForEOFAck(context.Background(), time.Second) {
		t.Fatal("batch B's EOF handshake must complete")
	}
}

// TestOutputManagerStaleHandshakeRefreshClosesPriorAck verifies that enable()
// releases every waiter on a signaled-but-unacknowledged handshake by closing
// the exact acknowledgement channel those waiters have snapshotted.
func TestOutputManagerStaleHandshakeRefreshClosesPriorAck(t *testing.T) {

	manager := newHandshakeTestManager()

	manager.enable()
	manager.signalEOF(manager.currentEpoch())

	manager.mu.Lock()
	staleAck := manager.eofAck
	manager.mu.Unlock()
	if staleAck == nil {
		t.Fatal("signaled handshake has no acknowledgement channel")
	}

	manager.enable()
	select {
	case <-staleAck:
	default:
		t.Fatal("refresh did not close the prior acknowledgement channel")
	}
}

// TestOutputManagerNeverSignalingJoinerBoundsDegradation documents the
// accepted trade-off for joiners that never signal EOF themselves (the
// output-aggregate/dmap class, excluded from the cat/grep/tail EOF epilogue):
// their enable() bumps the epoch, so a concurrently finishing cat's EOF
// signal is dropped and its ack wait times out — a bounded degradation, not
// a hang, and without data loss: everything in the lines channel remains
// deliverable to the reader.
func TestOutputManagerNeverSignalingJoinerBoundsDegradation(t *testing.T) {

	manager := newHandshakeTestManager()
	testUser := &userserver.User{Name: "output-never-signaling-joiner-test"}

	// The cat batch enables and captures its epoch for the EOF decision.
	manager.enable()
	catEpoch := manager.currentEpoch()

	// A dmap-style joiner enables (bumping the epoch) and will never signal.
	manager.enable()

	// The cat's stale signal is dropped and its ack wait times out — bounded.
	manager.signalEOF(catEpoch)
	if eof, _ := handshakeChannels(manager); eofClosed(eof) {
		t.Fatal("a never-signaling joiner must invalidate the cat's stale EOF signal")
	}
	if manager.waitForEOFAck(context.Background(), 20*time.Millisecond) {
		t.Fatal("the cat's ack wait must time out (bounded degradation), not report an ack")
	}

	// No data loss: output produced after the join is still delivered.
	mustEnqueueOutput(t, manager, 0, []byte("joiner-data"))
	buf := make([]byte, 64)
	n, handled := manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "joiner-data" {
		t.Fatalf("expected joiner data delivery, got handled=%v data=%q", handled, buf[:n])
	}
	if !manager.enabled() {
		t.Fatal("output mode must remain enabled for the joiner")
	}
}

// TestOutputManagerTryReadDrainsBufferWhenModeOff pins the defensive
// buffer-before-mode ordering in tryRead: remainder data of an already
// accepted payload is delivered even if output mode has been cleared in the
// meantime. The state is manufactured under the lock since the current code
// cannot reach it naturally (maybeAckEOFLocked only clears mode once the
// buffer is empty).
func TestOutputManagerTryReadDrainsBufferWhenModeOff(t *testing.T) {

	manager := &outputManager{}
	testUser := &userserver.User{Name: "output-buffer-mode-off-test"}

	manager.mu.Lock()
	manager.mode = false
	manager.buffer = generatedOutput{generation: 7, payload: []byte("rest"), retainedBytes: len("rest")}
	manager.bufferedBytes = len(manager.buffer.payload)
	manager.retainedBytes = len(manager.buffer.payload)
	manager.bufferedEntries = 1
	manager.mu.Unlock()

	buf := make([]byte, 64)
	n, handled := manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "rest" {
		t.Fatalf("expected buffered remainder despite mode off, got handled=%v data=%q", handled, buf[:n])
	}
}

// TestOutputManagerTryReadBuffersRemainder verifies that a payload larger than
// the read buffer is delivered across multiple tryRead calls via the internal
// remainder buffer.
func TestOutputManagerTryReadBuffersRemainder(t *testing.T) {

	manager := &outputManager{}
	testUser := &userserver.User{Name: "output-remainder-test"}

	manager.enable()
	mustEnqueueOutput(t, manager, 0, []byte("abcdefgh"))

	buf := make([]byte, 4)
	n, handled := manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "abcd" {
		t.Fatalf("expected first chunk %q, got handled=%v data=%q", "abcd", handled, buf[:n])
	}

	n, handled = manager.tryRead(buf, testUser, nil)
	if !handled || string(buf[:n]) != "efgh" {
		t.Fatalf("expected buffered remainder %q, got handled=%v data=%q", "efgh", handled, buf[:n])
	}
}
