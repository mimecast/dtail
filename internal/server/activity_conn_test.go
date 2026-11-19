package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// testWaitLimit only guards against hangs; no assertion depends on it.
	testWaitLimit = 5 * time.Second
	// realTimeout and realMinInterval keep real-clock tests short: the refresh
	// interval is realTimeout/4 = 50ms, so an idle close is due 300-350ms after
	// the last activity.
	realTimeout     = 200 * time.Millisecond
	realMinInterval = 10 * time.Millisecond
	// idleCloseSlack is the scheduling slack allowed past the documented idle
	// close window in real-clock tests (race detector, loaded CI hosts).
	idleCloseSlack = 500 * time.Millisecond
)

type fakeTicker struct {
	ch       chan time.Time
	stopped  chan struct{}
	stopOnce sync.Once
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
}

func (t *fakeTicker) Chan() <-chan time.Time { return t.ch }

func (t *fakeTicker) Stop() { t.stopOnce.Do(func() { close(t.stopped) }) }

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// deadlineRecorder records deadlines instead of applying them, so fake clock
// values never expire real pipe I/O.
type deadlineRecorder struct {
	net.Conn

	mu        sync.Mutex
	deadlines []time.Time
	err       error
}

func (c *deadlineRecorder) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.deadlines = append(c.deadlines, t)
	return nil
}

func (c *deadlineRecorder) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *deadlineRecorder) snapshot() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deadlines...)
}

type activityHarness struct {
	conn   *activityConn
	peer   net.Conn
	rec    *deadlineRecorder
	ticker *fakeTicker
	clock  *fakeClock
	// handled only wakes a waiter; the tokens carry no identity, so waits are
	// decided by the counters below, not by receiving a token.
	handled chan struct{}
	// handledTicks counts ticks the refresher finished; sentTicks counts ticks
	// this harness delivered. Only the test goroutine touches sentTicks.
	handledTicks atomic.Int64
	sentTicks    int64
	tickerCalls  int
	interval     time.Duration
}

func newActivityHarness(t *testing.T) *activityHarness {
	t.Helper()
	local, peer := net.Pipe()
	h := &activityHarness{
		peer:    peer,
		rec:     &deadlineRecorder{Conn: local},
		ticker:  newFakeTicker(),
		clock:   &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		handled: make(chan struct{}, 1),
	}
	h.conn = newActivityConn(h.rec)
	h.conn.now = h.clock.Now
	h.conn.newTicker = func(interval time.Duration) activityTicker {
		h.tickerCalls++
		h.interval = interval
		return h.ticker
	}
	// The hook runs on the refresher goroutine, which must never block on the
	// test (see activityConn.tickHandled), so it records the handled tick and
	// then wakes a waiter only if one is listening. tick waits on the counter
	// rather than on a token, so a token left over by a tick nobody waited for
	// cannot end a later wait early, and no token can be drained by mistake.
	h.conn.tickHandled = func() {
		h.handledTicks.Add(1)
		select {
		case h.handled <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		_ = h.conn.Close()
		_ = peer.Close()
	})
	return h
}

func (h *activityHarness) enable(t *testing.T, ctx context.Context, timeout time.Duration) {
	t.Helper()
	if err := h.conn.enable(ctx, timeout); err != nil {
		t.Fatalf("enable(%v): %v", timeout, err)
	}
}

// extension returns the deadline extension for timeout at the harness interval.
func (h *activityHarness) extension(timeout time.Duration) time.Duration {
	return idleDeadlineExtension(timeout, h.interval)
}

// tick delivers one tick and returns only after the refresher finished
// processing it, so the test's next I/O or clock change cannot race with it.
func (h *activityHarness) tick(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	select {
	case h.ticker.ch <- h.clock.Now():
	case <-timer.C:
		t.Fatal("refresher did not receive tick")
	}
	h.sentTicks++
	// The refresher handles ticks one at a time and in delivery order, so
	// waiting for the handled count to reach the number of ticks delivered
	// waits for exactly this tick, even when an earlier tick was delivered
	// without waiting for it. Tests that deliver a tick directly keep the
	// count honest by incrementing sentTicks themselves.
	for h.handledTicks.Load() < h.sentTicks {
		select {
		case <-h.handled:
		case <-timer.C:
			t.Fatal("refresher did not finish handling tick")
		}
	}
}

func (h *activityHarness) waitStopped(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	select {
	case <-h.ticker.stopped:
	case <-timer.C:
		t.Fatal("refresher did not stop")
	}
}

func (h *activityHarness) write(t *testing.T) {
	t.Helper()
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(h.peer, make([]byte, 1))
		readErr <- err
	}()
	if _, err := h.conn.Write([]byte{'x'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-readErr; err != nil {
		t.Fatalf("peer read: %v", err)
	}
}

func (h *activityHarness) read(t *testing.T) {
	t.Helper()
	writeErr := make(chan error, 1)
	go func() {
		_, err := h.peer.Write([]byte{'x'})
		writeErr <- err
	}()
	if _, err := io.ReadFull(h.conn, make([]byte, 1)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("peer write: %v", err)
	}
}

func (h *activityHarness) wantDeadlines(t *testing.T, want ...time.Time) {
	t.Helper()
	got := h.rec.snapshot()
	if len(got) != len(want) {
		t.Fatalf("deadlines = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("deadline[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestIdleRefreshInterval(t *testing.T) {
	tests := []struct {
		timeout     time.Duration
		minInterval time.Duration
		want        time.Duration
	}{
		{timeout: time.Millisecond, minInterval: minIdleRefreshInterval, want: time.Second},
		{timeout: time.Second, minInterval: minIdleRefreshInterval, want: time.Second},
		{timeout: 4 * time.Second, minInterval: minIdleRefreshInterval, want: time.Second},
		{timeout: 8 * time.Second, minInterval: minIdleRefreshInterval, want: 2 * time.Second},
		{timeout: time.Minute, minInterval: minIdleRefreshInterval, want: 15 * time.Second},
		{timeout: 2 * time.Minute, minInterval: minIdleRefreshInterval, want: 30 * time.Second},
		{timeout: 15 * time.Minute, minInterval: minIdleRefreshInterval, want: 30 * time.Second},
		{timeout: realTimeout, minInterval: realMinInterval, want: 50 * time.Millisecond},
		{timeout: 20 * time.Millisecond, minInterval: realMinInterval, want: realMinInterval},
	}
	for _, tt := range tests {
		if got := idleRefreshInterval(tt.timeout, tt.minInterval); got != tt.want {
			t.Errorf("idleRefreshInterval(%v, %v) = %v, want %v",
				tt.timeout, tt.minInterval, got, tt.want)
		}
	}
}

func TestActivityConnEnableSetsInitialDeadline(t *testing.T) {
	h := newActivityHarness(t)
	const timeout = time.Minute
	h.enable(t, t.Context(), timeout)

	if h.tickerCalls != 1 || h.interval != 15*time.Second {
		t.Fatalf("ticker calls = %d interval = %v, want 1 call with 15s", h.tickerCalls, h.interval)
	}
	// timeout + 2 * interval
	h.wantDeadlines(t, h.clock.Now().Add(90*time.Second))
}

func TestActivityConnActivityExtendsDeadlineOnTick(t *testing.T) {
	tests := []struct {
		name     string
		activity func(*activityHarness, *testing.T)
	}{
		{name: "read", activity: (*activityHarness).read},
		{name: "write", activity: (*activityHarness).write},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newActivityHarness(t)
			const timeout = time.Minute
			h.enable(t, t.Context(), timeout)
			initial := h.clock.Now().Add(h.extension(timeout))

			tt.activity(h, t)
			// I/O itself must not touch the deadline.
			h.wantDeadlines(t, initial)

			h.clock.advance(h.interval)
			h.tick(t)
			h.wantDeadlines(t, initial, h.clock.Now().Add(h.extension(timeout)))
		})
	}
}

func TestActivityConnIdleTicksDoNotExtendDeadline(t *testing.T) {
	h := newActivityHarness(t)
	const timeout = time.Minute
	h.enable(t, t.Context(), timeout)
	initial := h.clock.Now().Add(h.extension(timeout))

	for range 5 {
		h.clock.advance(h.interval)
		h.tick(t)
	}
	h.wantDeadlines(t, initial)
}

func TestActivityConnActivityBeforeEnableIsNotCarriedOver(t *testing.T) {
	h := newActivityHarness(t)
	h.write(t) // handshake traffic
	h.enable(t, t.Context(), time.Minute)
	initial := h.rec.snapshot()

	h.tick(t)
	h.wantDeadlines(t, initial...)
}

func TestActivityConnActivityIsConsumedOncePerTick(t *testing.T) {
	h := newActivityHarness(t)
	const timeout = time.Minute
	h.enable(t, t.Context(), timeout)
	initial := h.clock.Now().Add(h.extension(timeout))

	// A burst of activity just before a tick yields exactly one refresh.
	for range 10 {
		h.write(t)
	}
	h.clock.advance(time.Second)
	h.tick(t)
	refreshed := h.clock.Now().Add(h.extension(timeout))
	h.wantDeadlines(t, initial, refreshed)

	// The flag was cleared: the following idle tick must not extend again.
	h.clock.advance(h.interval)
	h.tick(t)
	h.wantDeadlines(t, initial, refreshed)

	// New activity after the tick is picked up by the next one.
	h.read(t)
	h.clock.advance(h.interval)
	h.tick(t)
	h.wantDeadlines(t, initial, refreshed, h.clock.Now().Add(h.extension(timeout)))
}

// TestActivityConnLateTickRefreshesBeforeDeadline drives the worst case for an
// active session: activity just before a tick, an idle gap just under the
// timeout, and the tick after the next activity delivered late. The former
// timeout + interval extension left zero scheduling margin at every timeout,
// not only at timeouts that are not a multiple of the interval: one interval
// late, 900s, 61s, 5s and 2s land exactly on the deadline (the refresh races
// the poller that enforces it) and 901s misses it outright, by 29s. Timeout mod
// interval only decides how badly a late tick misses, never whether it misses.
func TestActivityConnLateTickRefreshesBeforeDeadline(t *testing.T) {
	timeouts := []time.Duration{
		901 * time.Second, 900 * time.Second, 61 * time.Second, 5 * time.Second,
		2 * time.Second, // at the minimum interval
	}
	for _, timeout := range timeouts {
		for _, lateIntervals := range []time.Duration{0, 1} {
			name := fmt.Sprintf("timeout=%v/late=%dintervals", timeout, lateIntervals)
			t.Run(name, func(t *testing.T) {
				testLateTickRefreshesBeforeDeadline(t, timeout, lateIntervals)
			})
		}
	}
}

func testLateTickRefreshesBeforeDeadline(t *testing.T, timeout, lateIntervals time.Duration) {
	h := newActivityHarness(t)
	start := h.clock.Now()
	h.enable(t, t.Context(), timeout)
	interval := h.interval
	extension := h.extension(timeout)
	at := start.Add

	// Activity just before the first tick; the tick refreshes the deadline.
	h.clock.set(at(interval - time.Millisecond))
	h.write(t)
	h.clock.set(at(interval))
	h.tick(t)
	deadline := at(interval + extension)
	h.wantDeadlines(t, at(extension), deadline)

	// Idle ticks on schedule until the next activity, just under timeout later.
	activity := interval + timeout - 100*time.Millisecond
	next := 2 * interval
	for ; next <= activity; next += interval {
		h.clock.set(at(next))
		h.tick(t)
	}
	h.clock.set(at(activity))
	h.write(t)

	// The first tick due after the activity is handled late.
	handled := at(next + lateIntervals*interval)
	if !handled.Before(deadline) {
		t.Fatalf("late tick at %v is not before deadline %v; active session would close",
			handled.Sub(start), deadline.Sub(start))
	}
	h.clock.set(handled)
	h.tick(t)
	h.wantDeadlines(t, at(extension), deadline, handled.Add(extension))
}

// TestActivityConnSmallTimeoutFloorsTheWindow pins the behaviour documented at
// config.DefaultIdleSessionTimeoutS for timeouts below
// idleRefreshDivisor*minIdleRefreshInterval: the interval floor stops the
// refresher from ticking sub-second, so the idle close window is the timeout
// plus two to three whole seconds rather than a proportional share of it. The
// active session guarantee still holds at the floor.
func TestActivityConnSmallTimeoutFloorsTheWindow(t *testing.T) {
	h := newActivityHarness(t)
	const timeout = 2 * time.Second
	start := h.clock.Now()
	h.enable(t, t.Context(), timeout)

	if h.interval != minIdleRefreshInterval {
		t.Fatalf("interval = %v, want the %v floor", h.interval, minIdleRefreshInterval)
	}
	if got, want := h.extension(timeout), timeout+2*time.Second; got != want {
		t.Fatalf("extension = %v, want %v (floored, not %v)", got, want, timeout*3/2)
	}
	initial := start.Add(h.extension(timeout))
	h.wantDeadlines(t, initial)

	// Activity just under one timeout after the last handled tick is still
	// picked up in time: the tick that follows it refreshes before the
	// deadline above.
	lastActivity := start.Add(timeout - 100*time.Millisecond)
	h.clock.set(lastActivity)
	h.write(t)
	h.clock.set(start.Add(2 * h.interval))
	h.tick(t)
	refreshed := start.Add(2*h.interval + h.extension(timeout))
	h.wantDeadlines(t, initial, refreshed)

	// Idle from here on, so that deadline is the one that closes the
	// connection; it must land inside the documented window.
	for next := 3 * h.interval; next <= 6*time.Second; next += h.interval {
		h.clock.set(start.Add(next))
		h.tick(t)
	}
	h.wantDeadlines(t, initial, refreshed)

	idleFor := refreshed.Sub(lastActivity)
	earliest, latest := timeout+2*h.interval, timeout+3*h.interval
	if idleFor < earliest || idleFor > latest {
		t.Fatalf("idle close %v after last activity, want within [%v, %v]",
			idleFor, earliest, latest)
	}
}

func TestActivityConnCloseStopsRefresher(t *testing.T) {
	h := newActivityHarness(t)
	h.enable(t, t.Context(), time.Minute)
	h.write(t)

	if err := h.conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-h.ticker.stopped:
	default:
		t.Fatal("Close returned before the refresher stopped")
	}
	if len(h.rec.snapshot()) != 1 {
		t.Fatalf("deadlines = %v, want no refresh after Close", h.rec.snapshot())
	}
	if err := h.conn.enable(t.Context(), time.Minute); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("enable after Close = %v, want net.ErrClosed", err)
	}
}

func TestActivityConnContextCancelStopsRefresher(t *testing.T) {
	h := newActivityHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.enable(t, ctx, time.Minute)

	h.write(t)
	cancel()
	h.waitStopped(t)
	if len(h.rec.snapshot()) != 1 {
		t.Fatalf("deadlines = %v, want no refresh after cancel", h.rec.snapshot())
	}
}

func TestActivityConnSetDeadlineErrorStopsRefresher(t *testing.T) {
	h := newActivityHarness(t)
	h.enable(t, t.Context(), time.Minute)

	h.rec.setErr(net.ErrClosed)
	h.write(t)
	h.tick(t)
	h.waitStopped(t)
}

func TestActivityConnDisabledTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		h := newActivityHarness(t)
		h.enable(t, t.Context(), timeout)

		if h.tickerCalls != 0 {
			t.Fatalf("timeout %v started a refresher", timeout)
		}
		h.wantDeadlines(t, time.Time{})
		h.write(t)
		if err := h.conn.Close(); err != nil {
			t.Fatalf("close with timeout %v: %v", timeout, err)
		}
	}
}

func TestActivityConnEnableTwiceFails(t *testing.T) {
	h := newActivityHarness(t)
	h.enable(t, t.Context(), time.Minute)
	if err := h.conn.enable(t.Context(), time.Minute); !errors.Is(err, errIdleDeadlineEnabled) {
		t.Fatalf("second enable = %v, want errIdleDeadlineEnabled", err)
	}
	if h.tickerCalls != 1 {
		t.Fatalf("ticker calls = %d, want 1", h.tickerCalls)
	}
}

func TestActivityConnConcurrentActivityTicksAndClose(t *testing.T) {
	h := newActivityHarness(t)
	// Ticks are sent freely below, so nothing waits for tick completion.
	h.conn.tickHandled = nil
	h.enable(t, t.Context(), time.Minute)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.Discard, h.peer)
	}()
	go func() {
		defer wg.Done()
		for {
			if _, err := h.conn.Write([]byte("payload")); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case h.ticker.ch <- time.Time{}:
			case <-h.ticker.stopped:
				return
			}
		}
	}()

	// Bounded like every other wait in this file: if the refresher ever stops
	// refreshing, fail within testWaitLimit instead of spinning forever.
	deadline := time.Now().Add(testWaitLimit)
	for len(h.rec.snapshot()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("refresher recorded %d deadlines within %v, want at least 3",
				len(h.rec.snapshot()), testWaitLimit)
		}
		h.clock.advance(time.Second)
		// Yield so the refresher still makes progress under GOMAXPROCS=1.
		runtime.Gosched()
	}
	if err := h.conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	afterClose := len(h.rec.snapshot())
	wg.Wait()
	if got := len(h.rec.snapshot()); got != afterClose {
		t.Fatalf("deadlines grew from %d to %d after Close returned", afterClose, got)
	}
}

// newRealActivityConn returns an activityConn over one end of a net.Pipe with
// real deadlines and ticker, and the short test refresh interval.
func newRealActivityConn(t *testing.T) (*activityConn, net.Conn) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	conn := newActivityConn(serverConn)
	conn.minInterval = realMinInterval
	t.Cleanup(func() {
		_ = conn.Close()
		_ = clientConn.Close()
	})
	return conn, clientConn
}

// wantIdleClose asserts err is a timeout that happened within the documented
// idle close window, [timeout + 2*interval, timeout + 3*interval), after the
// last activity, allowing idleCloseSlack for late scheduling.
func wantIdleClose(t *testing.T, err error, sinceLastActivity time.Duration) {
	t.Helper()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("idle read error = %v, want network timeout", err)
	}
	interval := idleRefreshInterval(realTimeout, realMinInterval)
	earliest := idleDeadlineExtension(realTimeout, interval)
	latest := earliest + interval + idleCloseSlack
	if sinceLastActivity < earliest || sinceLastActivity > latest {
		t.Fatalf("idle close %v after last activity, want within [%v, %v]",
			sinceLastActivity, earliest, latest)
	}
}

func TestActivityConnTimesOutWhenIdle(t *testing.T) {
	conn, _ := newRealActivityConn(t)

	// Nothing happens after enable, so enable is the last activity.
	started := time.Now()
	if err := conn.enable(t.Context(), realTimeout); err != nil {
		t.Fatalf("enable idle timeout: %v", err)
	}
	_, err := conn.Read(make([]byte, 1))
	wantIdleClose(t, err, time.Since(started))
}

// TestActivityConnWritesKeepBlockedReadAlive covers the SSH server shape
// during a long download: the read side is blocked while only writes happen.
// Real deadlines apply, so write-only activity must keep refreshing the shared
// deadline, and the blocked read must time out once writes stop.
func TestActivityConnWritesKeepBlockedReadAlive(t *testing.T) {
	const (
		writeEvery = realTimeout / 4
		activeFor  = time.Second // several deadline extensions of 300ms
	)
	conn, peer := newRealActivityConn(t)
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	if err := conn.enable(t.Context(), realTimeout); err != nil {
		t.Fatalf("enable idle timeout: %v", err)
	}

	readErr := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readErr <- err
	}()

	ticker := time.NewTicker(writeEvery)
	defer ticker.Stop()
	started := time.Now()
	var lastWrite time.Time
	for time.Since(started) < activeFor {
		select {
		case err := <-readErr:
			t.Fatalf("blocked read ended after %v while writes continued: %v",
				time.Since(started), err)
		case <-ticker.C:
		}
		lastWrite = time.Now()
		if _, err := conn.Write([]byte{'x'}); err != nil {
			t.Fatalf("write after %v: %v", time.Since(started), err)
		}
	}
	ticker.Stop()

	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	select {
	case err := <-readErr:
		wantIdleClose(t, err, time.Since(lastWrite))
	case <-timer.C:
		t.Fatal("blocked read did not time out after writes stopped")
	}
}
