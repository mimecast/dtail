package connectors

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
)

// gatedAckHandler is a mockHandler whose first session acknowledgement is
// held back until gate is closed, as a paused output holds back the
// acknowledgement of a SESSION START in flight; waiting is closed once the
// start waits for it. The second acknowledgement (a deferred rollback's
// update) is held back the same way by followUpGate, closed by default;
// followUpWaiting is closed once it is waited for.
type gatedAckHandler struct {
	*mockHandler
	mu              sync.Mutex
	waits           int
	gate            chan struct{}
	waiting         chan struct{}
	followUpGate    chan struct{}
	followUpWaiting chan struct{}
}

func newGatedAckHandler(acks ...handlers.SessionAck) *gatedAckHandler {
	followUpGate := make(chan struct{})
	close(followUpGate)
	return &gatedAckHandler{
		mockHandler:     &mockHandler{waitForCapabilities: true, sessionAcks: acks},
		gate:            make(chan struct{}),
		waiting:         make(chan struct{}),
		followUpGate:    followUpGate,
		followUpWaiting: make(chan struct{}),
	}
}

func (h *gatedAckHandler) SendMessage(command string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mockHandler.SendMessage(command)
}

func (h *gatedAckHandler) WaitForSessionAck(timeout, maxWait time.Duration) (handlers.SessionAck, bool) {
	h.mu.Lock()
	if timeout <= 0 {
		// drainSessionAcks: nothing is queued.
		defer h.mu.Unlock()
		return h.mockHandler.WaitForSessionAck(timeout, maxWait)
	}
	h.waits++
	wait := h.waits
	waiting, gate := h.waiting, h.gate
	if wait > 1 {
		waiting, gate = h.followUpWaiting, h.followUpGate
	}
	h.mu.Unlock()
	if wait <= 2 {
		close(waiting)
	}
	<-gate
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mockHandler.WaitForSessionAck(timeout, maxWait)
}

// sentCommands returns the commands the handler was sent so far.
func (h *gatedAckHandler) sentCommands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.commands...)
}

// abandonCalls records the abandon callbacks of restoreStartSpec.
type abandonCalls []bool

func (c *abandonCalls) record(abandoned bool) { *c = append(*c, abandoned) }

// abandoningHandler is a gatedAckHandler that aggregates, as a
// handlers.MaprHandler: it is told when its start's workload is abandoned.
type abandoningHandler struct {
	*gatedAckHandler
	abandoned abandonCalls
}

func (h *abandoningHandler) AbandonStartWorkload(abandoned bool) { h.abandoned.record(abandoned) }

// TestRestoreStartSessionSpecTellsAggregatingHandler: the connection's
// deferred rollback reaches a handler that aggregates, and a handler that
// does not is left alone.
func TestRestoreStartSessionSpecTellsAggregatingHandler(t *testing.T) {
	handler := &abandoningHandler{gatedAckHandler: newGatedAckHandler(
		handlers.SessionAck{Action: "start", Generation: 1}, handlers.SessionAck{Action: "update", Generation: 2})}
	conn := &ServerConnection{server: "srv1", handler: handler}
	conn.sessionState.updateAckMaxWait = 50 * time.Millisecond
	if replaced, err := conn.ReplaceStartSessionSpec(startSpecReloaded); !replaced || err != nil {
		t.Fatalf("ReplaceStartSessionSpec() = %v, %v, want true", replaced, err)
	}
	startErr := make(chan error, 1)
	go func() {
		startErr <- startInitialSession(context.Background(), "srv1", handler, &conn.sessionState,
			startSpecInitial, logging.NopLogger{})
	}()
	<-handler.waiting

	if _, err := conn.RestoreStartSessionSpec(startSpecInitial); !errors.Is(err, ErrSessionRollbackDeferred) {
		t.Fatalf("RestoreStartSessionSpec() error = %v, want ErrSessionRollbackDeferred", err)
	}
	if !slices.Equal(handler.abandoned, abandonCalls{true}) {
		t.Fatalf("abandon calls = %v, want one true", handler.abandoned)
	}
	close(handler.gate)
	if err := <-startErr; err != nil {
		t.Fatalf("startInitialSession() error = %v", err)
	}

	// A handler without AbandonStartWorkload gets a callback doing nothing.
	abandonStartWorkload(newGatedAckHandler())(true)
}

// startWithReloadedSpec replaces the start spec of state with
// startSpecReloaded, as a :reload does while the connection is still
// connecting, and starts the session on handler in the background; it
// returns once the start waits for its acknowledgement.
func startWithReloadedSpec(t *testing.T, handler *gatedAckHandler, state *committedSessionState) <-chan error {
	t.Helper()
	if replaced, err := state.replaceStartSpec(startSpecReloaded); !replaced || err != nil {
		t.Fatalf("replaceStartSpec() = %v, %v before the start, want true", replaced, err)
	}
	startErr := make(chan error, 1)
	go func() {
		startErr <- startInitialSession(context.Background(), "srv1", handler, state, startSpecInitial, logging.NopLogger{})
	}()
	<-handler.waiting
	return startErr
}

// TestRestoreStartSpecDeferredUntilStartCommits: a :reload replaced the
// start spec of a connecting server, its SESSION START began with the
// reloaded spec, and the reload failed while that start still waited for
// its acknowledgement (longer than the control loop's cap). The rollback
// can not wait for it (ErrSessionRollbackDeferred), but the start must then
// update its session back to the previous spec right after it committed,
// instead of leaving the server on the reloaded spec while the client and
// the other servers are on the previous one.
func TestRestoreStartSpecDeferredUntilStartCommits(t *testing.T) {
	handler := newGatedAckHandler(handlers.SessionAck{Action: "start", Generation: 1},
		handlers.SessionAck{Action: "update", Generation: 2})
	state := committedSessionState{updateAckMaxWait: 50 * time.Millisecond}
	startErr := startWithReloadedSpec(t, handler, &state)

	var abandoned abandonCalls
	restored, err := state.restoreStartSpec(startSpecInitial, abandoned.record)
	if restored || !errors.Is(err, ErrSessionRollbackDeferred) {
		t.Fatalf("restoreStartSpec() = %v, %v during the start, want ErrSessionRollbackDeferred", restored, err)
	}
	// The handler is told that the start's workload is abandoned.
	if !slices.Equal(abandoned, abandonCalls{true}) {
		t.Fatalf("abandon calls = %v, want one true", abandoned)
	}
	close(handler.gate)
	if err := <-startErr; err != nil {
		t.Fatalf("startInitialSession() error = %v", err)
	}
	if spec, generation, committed := state.snapshot(); !committed || generation != 2 || spec.Regex != "ERROR" {
		t.Fatalf("committed %v generation %d spec %+v, want the previous spec at 2", committed, generation, spec)
	}
	commands := handler.sentCommands()
	if len(commands) != 2 || !strings.HasPrefix(commands[0], "SESSION START ") ||
		!strings.HasPrefix(commands[1], "SESSION UPDATE 2 ") {
		t.Fatalf("commands = %q, want the start and an update at generation 2", commands)
	}
}

// TestDeferredStartRollbackFailureFailsStart: a start whose deferred
// rollback is not acknowledged fails, so its connection ends instead of
// running the reloaded spec the client rolled back.
func TestDeferredStartRollbackFailureFailsStart(t *testing.T) {
	handler := newGatedAckHandler(handlers.SessionAck{Action: "start", Generation: 1})
	state := committedSessionState{updateAckMaxWait: 50 * time.Millisecond}
	startErr := startWithReloadedSpec(t, handler, &state)

	if _, err := state.restoreStartSpec(startSpecInitial, func(bool) {}); !errors.Is(err, ErrSessionRollbackDeferred) {
		t.Fatalf("restoreStartSpec() error = %v, want ErrSessionRollbackDeferred", err)
	}
	close(handler.gate)
	if err := <-startErr; !errors.Is(err, ErrSessionAckTimeout) {
		t.Fatalf("startInitialSession() error = %v, want the rollback's ErrSessionAckTimeout", err)
	}
}

// TestRestoreStartSpecAfterStartFinished: a rollback after the start was
// acknowledged is not deferred: it reports false, so the caller updates
// the committed session, and the start sends nothing more.
func TestRestoreStartSpecAfterStartFinished(t *testing.T) {
	handler := newGatedAckHandler(handlers.SessionAck{Action: "start", Generation: 1})
	var state committedSessionState
	startErr := startWithReloadedSpec(t, handler, &state)
	close(handler.gate)
	if err := <-startErr; err != nil {
		t.Fatalf("startInitialSession() error = %v", err)
	}

	var abandoned abandonCalls
	if restored, err := state.restoreStartSpec(startSpecInitial, abandoned.record); restored || err != nil {
		t.Fatalf("restoreStartSpec() = %v, %v after the start, want false, nil", restored, err)
	}
	if len(abandoned) != 0 {
		t.Fatalf("abandon calls = %v for a start that had finished, want none", abandoned)
	}
	if spec, generation, _ := state.snapshot(); generation != 1 || spec.Regex != "WARN" {
		t.Fatalf("generation %d spec %+v, want the started spec at 1 for the caller to update", generation, spec)
	}
	if commands := handler.sentCommands(); len(commands) != 1 {
		t.Fatalf("commands = %q, want the start only", commands)
	}
}

// TestRestoreStartSpecBeforeStart: before the start began the rollback just
// replaces the start spec back.
func TestRestoreStartSpecBeforeStart(t *testing.T) {
	var state committedSessionState
	if replaced, err := state.replaceStartSpec(startSpecReloaded); !replaced || err != nil {
		t.Fatalf("replaceStartSpec() = %v, %v, want true", replaced, err)
	}
	var abandoned abandonCalls
	if restored, err := state.restoreStartSpec(startSpecInitial, abandoned.record); !restored || err != nil {
		t.Fatalf("restoreStartSpec() = %v, %v, want true", restored, err)
	}
	if len(abandoned) != 0 {
		t.Fatalf("abandon calls = %v for a start that had not begun, want none", abandoned)
	}
	if state.startSpec.Regex != "ERROR" {
		t.Fatalf("start spec %+v, want the previous spec", state.startSpec)
	}
}

// TestDeferredStartRollbackNotCommittedUntilAcknowledged: while the
// deferred rollback's update of a start that committed the reloaded spec
// waits for its acknowledgement, the session does not count as committed.
// A second :reload arriving meanwhile must not take it for an active
// session at the reloaded spec: it would record that spec for its own
// rollback, wait for the lock, update the session on top of the rolled
// back one and, should it fail, roll it back to the failed reload's spec
// while the client is on the previous one. It takes the session for one
// still connecting instead (ReplaceStartSessionSpec), which waits for the
// follow-up and then finds the start begun and the previous spec
// committed, to update on top of.
func TestDeferredStartRollbackNotCommittedUntilAcknowledged(t *testing.T) {
	handler := newGatedAckHandler(handlers.SessionAck{Action: "start", Generation: 1},
		handlers.SessionAck{Action: "update", Generation: 2})
	handler.followUpGate = make(chan struct{})
	state := committedSessionState{updateAckMaxWait: 50 * time.Millisecond}
	startErr := startWithReloadedSpec(t, handler, &state)
	if _, err := state.restoreStartSpec(startSpecInitial, func(bool) {}); !errors.Is(err, ErrSessionRollbackDeferred) {
		t.Fatalf("restoreStartSpec() error = %v, want ErrSessionRollbackDeferred", err)
	}
	close(handler.gate)
	<-handler.followUpWaiting

	// The start committed the reloaded spec; its rollback is in flight.
	if spec, _, committed := state.snapshot(); committed {
		t.Fatalf("snapshot() = committed %+v during the deferred rollback, want not committed", spec)
	}
	if replaced, err := state.replaceStartSpec(startSpecInitial); replaced || !errors.Is(err, ErrSessionStartPending) {
		t.Fatalf("replaceStartSpec() = %v, %v during the deferred rollback, want ErrSessionStartPending", replaced, err)
	}
	close(handler.followUpGate)
	if err := <-startErr; err != nil {
		t.Fatalf("startInitialSession() error = %v", err)
	}
	if replaced, err := state.replaceStartSpec(startSpecInitial); replaced || err != nil {
		t.Fatalf("replaceStartSpec() = %v, %v after the rollback, want false, nil", replaced, err)
	}
	if spec, generation, committed := state.snapshot(); !committed || generation != 2 || spec.Regex != "ERROR" {
		t.Fatalf("committed %v generation %d spec %+v, want the previous spec at 2", committed, generation, spec)
	}
}

// TestRestoreStartSpecStartFinishedWhileGivingUp: the start finishes right
// after the rollback gave up waiting for the lock, before it could defer
// a follow-up. The rollback then waits for the lock once more and reports
// false, so the caller updates the committed session, and defers nothing.
func TestRestoreStartSpecStartFinishedWhileGivingUp(t *testing.T) {
	const maxWait = 100 * time.Millisecond
	state := committedSessionState{updateAckMaxWait: maxWait}
	state.applyMu.Lock()
	state.started = true
	state.finishStart()
	go func() {
		time.Sleep(maxWait + maxWait/2)
		state.applyMu.Unlock()
	}()

	var abandoned abandonCalls
	if restored, err := state.restoreStartSpec(startSpecInitial, abandoned.record); restored || err != nil {
		t.Fatalf("restoreStartSpec() = %v, %v, want false, nil", restored, err)
	}
	// Told before the deferral was tried, and taken back as it was not
	// deferred: the caller rolls the committed session back by an update.
	if !slices.Equal(abandoned, abandonCalls{true, false}) {
		t.Fatalf("abandon calls = %v, want true then false", abandoned)
	}
	if _, deferred := state.finishStart(); deferred {
		t.Fatal("restoreStartSpec() deferred a follow-up after the start finished")
	}
}
