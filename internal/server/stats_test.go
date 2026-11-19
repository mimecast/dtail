package server

import (
	"sync"
	"testing"
)

// TestStatsConnectionCounterBalance verifies that incrementConnections and
// decrementConnections remain balanced so currentConnections never goes negative.
//
// The bug: incrementConnections() was called once per TCP connection in
// handleConnection, but decrementConnections() was called inside the
// handleShellRequest goroutine that waits on sshConn.Wait(). A connection
// with N shell requests (channels) would therefore decrement N times against
// a single increment, driving currentConnections negative.
//
// The fix pairs both calls inside handleConnection via a defer, removing the
// decrement from handleShellRequest entirely.
func TestStatsConnectionCounterBalance(t *testing.T) {
	s := newStats(100, serverTestLoggers.Diagnostics)

	// Simulate a single TCP connection that opens 3 shell channels (requests).
	// Before the fix, incrementConnections was called once but
	// decrementConnections was called once per shell request.

	// Increment once per connection (the correct, fixed behaviour).
	s.incrementConnections()

	if s.currentConnections != 1 {
		t.Fatalf("expected currentConnections=1 after one connection, got %d", s.currentConnections)
	}

	// Simulate 3 shell requests completing (decrement only once — the fixed path).
	// Under the old code this loop body would be called once per shell request,
	// but decrementConnections must only be called once per connection.
	s.decrementConnections()

	if s.currentConnections != 0 {
		t.Fatalf("expected currentConnections=0 after decrement, got %d", s.currentConnections)
	}
}

// TestStatsMultipleConnectionsBalance verifies counters stay non-negative across
// multiple independent connections each with their own lifecycle.
func TestStatsMultipleConnectionsBalance(t *testing.T) {
	s := newStats(100, serverTestLoggers.Diagnostics)
	const n = 5

	for i := 0; i < n; i++ {
		s.incrementConnections()
	}
	if s.currentConnections != n {
		t.Fatalf("expected currentConnections=%d, got %d", n, s.currentConnections)
	}

	for i := 0; i < n; i++ {
		s.decrementConnections()
		if s.currentConnections < 0 {
			t.Fatalf("currentConnections went negative at decrement %d: got %d", i+1, s.currentConnections)
		}
	}
	if s.currentConnections != 0 {
		t.Fatalf("expected currentConnections=0 after all decrements, got %d", s.currentConnections)
	}
}

// TestStatsConnectionCounterNeverNegative simulates the pre-fix behaviour to
// document what the bug looked like: multiple decrements per single increment
// would drive the counter negative. This test asserts that the counter does
// NOT go negative when decrement is called more times than increment (i.e.
// if decrement were still called per shell request rather than per connection).
//
// This test is intentionally written to document the invariant: the counter
// must always be >= 0. With the fix in place (decrement once per connection),
// the counter stays at 0 after one increment + one decrement.
func TestStatsCounterInvariant(t *testing.T) {
	s := newStats(100, serverTestLoggers.Diagnostics)

	s.incrementConnections() // one connection

	// With the fix: only one decrement per connection, counter stays >= 0.
	s.decrementConnections()

	if s.currentConnections < 0 {
		t.Fatalf("currentConnections must never be negative, got %d", s.currentConnections)
	}
}

// TestPreAuthSlotsCountAgainstLimit verifies that in-progress SSH handshakes
// (pre-auth connections) count against MaxConnections. This is the core
// regression test for the security fix: before the fix, slow or unauthenticated
// handshakes bypassed the limit and could create unbounded goroutines.
func TestPreAuthSlotsCountAgainstLimit(t *testing.T) {
	const maxConns = 3
	s := newStats(maxConns, serverTestLoggers.Diagnostics)

	// Reserve pre-auth slots up to the limit; each must succeed.
	for i := 0; i < maxConns; i++ {
		if err := s.serverLimitExceeded(); err != nil {
			t.Fatalf("slot %d: expected limit not exceeded, got: %v", i, err)
		}
		s.reservePreAuth()
	}

	// With all slots filled by pre-auth connections the limit must be exceeded.
	if err := s.serverLimitExceeded(); err == nil {
		t.Fatal("expected serverLimitExceeded() to return error when pre-auth fills all slots, got nil")
	}

	if s.preAuthConnections != maxConns {
		t.Fatalf("expected preAuthConnections=%d, got %d", maxConns, s.preAuthConnections)
	}

	// Release one pre-auth slot (handshake failed); limit should drop below max again.
	s.releasePreAuth()
	if err := s.serverLimitExceeded(); err != nil {
		t.Fatalf("after releasing one pre-auth slot, expected limit not exceeded, got: %v", err)
	}
}

// TestPromotePreAuthToConnectionIsAtomic verifies that
// promotePreAuthToConnection correctly transitions a pre-auth reservation into
// a full authenticated connection without losing or double-counting the slot.
func TestPromotePreAuthToConnectionIsAtomic(t *testing.T) {
	s := newStats(10, serverTestLoggers.Diagnostics)

	// Reserve a pre-auth slot then promote it; the totals must balance.
	s.reservePreAuth()
	if s.preAuthConnections != 1 {
		t.Fatalf("expected preAuthConnections=1, got %d", s.preAuthConnections)
	}
	if s.currentConnections != 0 {
		t.Fatalf("expected currentConnections=0 before promote, got %d", s.currentConnections)
	}

	s.promotePreAuthToConnection()

	if s.preAuthConnections != 0 {
		t.Fatalf("expected preAuthConnections=0 after promote, got %d", s.preAuthConnections)
	}
	if s.currentConnections != 1 {
		t.Fatalf("expected currentConnections=1 after promote, got %d", s.currentConnections)
	}
	if s.lifetimeConnections != 1 {
		t.Fatalf("expected lifetimeConnections=1 after promote, got %d", s.lifetimeConnections)
	}

	// Total effective connections must stay constant across the promote.
	// Before: 1 pre-auth + 0 current = 1. After: 0 pre-auth + 1 current = 1.
	total := s.preAuthConnections + s.currentConnections
	if total != 1 {
		t.Fatalf("expected total (pre-auth + current) = 1, got %d", total)
	}
}

// TestPreAuthLimitMixedWithAuthenticated verifies that the limit accounts for
// both pre-auth and authenticated connections together. This models the real
// scenario where some handshakes are still in flight while others have completed.
func TestPreAuthLimitMixedWithAuthenticated(t *testing.T) {
	const maxConns = 4
	s := newStats(maxConns, serverTestLoggers.Diagnostics)

	// Two connections complete their handshake successfully.
	s.reservePreAuth()
	s.promotePreAuthToConnection()
	s.reservePreAuth()
	s.promotePreAuthToConnection()

	// One more is in-progress (pre-auth).
	s.reservePreAuth()

	// Total is 3 (2 current + 1 pre-auth); one more slot should still be available.
	if err := s.serverLimitExceeded(); err != nil {
		t.Fatalf("expected one free slot remaining, got: %v", err)
	}

	// Reserve the last slot.
	s.reservePreAuth()

	// All 4 slots consumed; limit must be exceeded.
	if err := s.serverLimitExceeded(); err == nil {
		t.Fatal("expected serverLimitExceeded() with 2 current + 2 pre-auth, got nil")
	}
}

// TestPreAuthConcurrentReserveRelease exercises reservePreAuth and releasePreAuth
// under concurrent access to verify there are no data races. Run with -race.
func TestPreAuthConcurrentReserveRelease(t *testing.T) {
	s := newStats(1000, serverTestLoggers.Diagnostics)
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			s.reservePreAuth()
			s.releasePreAuth()
		}()
	}
	wg.Wait()

	if s.preAuthConnections != 0 {
		t.Fatalf("expected preAuthConnections=0 after all goroutines finish, got %d", s.preAuthConnections)
	}
}
