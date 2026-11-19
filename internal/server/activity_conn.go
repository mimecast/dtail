package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// idleRefreshDivisor sets the refresh tick to a fraction of the idle timeout.
	idleRefreshDivisor = 4
	// minIdleRefreshInterval keeps short timeouts from causing busy ticking. It
	// also floors the idle close window: a timeout below
	// idleRefreshDivisor*minIdleRefreshInterval closes two to three whole
	// seconds past the timeout instead of proportionally, and leaves the
	// refresher only one second of scheduling slack per tick. See
	// config.DefaultIdleSessionTimeoutS.
	minIdleRefreshInterval = time.Second
	// maxIdleRefreshInterval bounds how long past the idle timeout an idle
	// connection may stay open (at most three refresh intervals).
	maxIdleRefreshInterval = 30 * time.Second
	// idleDeadlineSlackIntervals is how many refresh intervals every deadline
	// adds on top of the idle timeout; see activityConn for why it is two.
	idleDeadlineSlackIntervals = 2
)

// errIdleDeadlineEnabled reports a second enable call on the same connection.
var errIdleDeadlineEnabled = errors.New("idle deadline already enabled")

// activityTicker is the part of *time.Ticker the deadline refresher uses. Tests
// inject a fake implementation to drive ticks deterministically.
type activityTicker interface {
	Chan() <-chan time.Time
	Stop()
}

// timeTicker adapts *time.Ticker to activityTicker.
type timeTicker struct {
	ticker *time.Ticker
}

// activityConn applies a rolling idle deadline after the SSH handshake.
//
// Read and Write only record activity in an atomic flag. A single refresher
// goroutine per connection wakes once per refresh interval and pushes the
// deadline forward when activity was recorded since the previous tick. This
// keeps clock reads and SetDeadline calls off the per-packet I/O path.
//
// Every deadline is set to now + timeout + 2*interval. Why this never closes an
// active session (every idle gap <= timeout): let a tick handled at h set the
// deadline D = h + timeout + 2*interval (enable counts as such a tick). The
// last activity it saw happened at or before h, so the next activity a is at
// most h + timeout. The first tick due after a is scheduled no later than
// a + interval; if it is handled less than one interval late (GC, CPU
// starvation, or ticks dropped by time.Ticker), it refreshes before
// a + 2*interval <= D. One interval covers tick granularity and the other is
// scheduling slack, independent of timeout mod interval.
//
// With ticks on schedule, an idle connection closes between timeout +
// 2*interval and timeout + 3*interval after its last activity (960-990 s for
// the default 900 s timeout); late ticks can only extend that.
type activityConn struct {
	net.Conn

	active      atomic.Bool
	now         func() time.Time
	newTicker   func(time.Duration) activityTicker
	minInterval time.Duration
	// tickHandled, when set before enable, runs on the refresher goroutine
	// after each tick is processed. Tests use it to synchronize; it is nil in
	// production. It must never block: while it runs the refresher can neither
	// take the next tick nor observe cancellation and exit, and Close waits for
	// that exit.
	tickHandled func()

	mu      sync.Mutex
	enabled bool
	closed  bool
	stop    chan struct{}
	done    chan struct{} // nil until the refresher starts; closed when it exits
}

func newActivityConn(conn net.Conn) *activityConn {
	return &activityConn{
		Conn:        conn,
		now:         time.Now,
		newTicker:   newTimeTicker,
		minInterval: minIdleRefreshInterval,
		stop:        make(chan struct{}),
	}
}

func newTimeTicker(interval time.Duration) activityTicker {
	return timeTicker{ticker: time.NewTicker(interval)}
}

// Chan returns the channel the ticks are delivered on.
func (t timeTicker) Chan() <-chan time.Time {
	return t.ticker.C
}

// Stop stops the underlying ticker.
func (t timeTicker) Stop() {
	t.ticker.Stop()
}

// Read reads from the connection and records activity on success.
func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.markActive()
	}
	return n, err
}

// Write writes to the connection and records activity on success.
func (c *activityConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.markActive()
	}
	return n, err
}

// Close stops the deadline refresher, waits for it to exit, and closes the
// underlying connection. No deadline is set after Close returns.
func (c *activityConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
	}
	done := c.done
	c.mu.Unlock()

	if done != nil {
		<-done
	}
	return c.Conn.Close()
}

// enable replaces the handshake deadline with a rolling idle deadline. The
// refresher stops when ctx is done, the connection is closed, or setting the
// deadline fails. enable may be called only once per connection.
//
// A timeout <= 0 clears the deadline and starts no refresher. That branch is
// defensive and unreachable in production: the only caller substitutes
// config.DefaultIdleSessionTimeoutS for a non-positive configured timeout, so
// no dtail.json can disable idle expiry. Only tests exercise it.
func (c *activityConn) enable(ctx context.Context, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case c.closed:
		return net.ErrClosed
	case c.enabled:
		return errIdleDeadlineEnabled
	}
	c.enabled = true

	if timeout <= 0 {
		if err := c.SetDeadline(time.Time{}); err != nil {
			return fmt.Errorf("clear idle deadline: %w", err)
		}
		return nil
	}

	interval := idleRefreshInterval(timeout, c.minInterval)
	extension := idleDeadlineExtension(timeout, interval)
	// Activity before enable is covered by the fresh deadline set here.
	c.active.Store(false)
	if err := c.SetDeadline(c.now().Add(extension)); err != nil {
		return fmt.Errorf("set idle deadline: %w", err)
	}

	c.done = make(chan struct{})
	go c.refreshLoop(ctx, c.newTicker(interval), extension, c.done)
	return nil
}

func (c *activityConn) markActive() {
	// Load first so steady I/O does not dirty the shared cache line per call.
	if !c.active.Load() {
		c.active.Store(true)
	}
}

func (c *activityConn) refreshLoop(ctx context.Context, ticker activityTicker,
	extension time.Duration, done chan<- struct{}) {

	defer close(done)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-ticker.Chan():
			keepRunning := c.refreshIfActive(ctx, extension)
			if c.tickHandled != nil {
				c.tickHandled()
			}
			if !keepRunning {
				return
			}
		}
	}
}

// refreshIfActive extends the deadline when activity was recorded since the
// previous tick. It reports whether the refresher should keep running.
func (c *activityConn) refreshIfActive(ctx context.Context, extension time.Duration) bool {
	// select picks randomly among ready cases; re-check shutdown so a tick
	// that raced with Close or cancellation never sets another deadline.
	select {
	case <-ctx.Done():
		return false
	case <-c.stop:
		return false
	default:
	}

	if !c.active.Swap(false) {
		return true
	}
	return c.SetDeadline(c.now().Add(extension)) == nil
}

// idleRefreshInterval returns the refresh tick for an idle timeout: a quarter
// of the timeout, clamped to [minInterval, maxIdleRefreshInterval].
func idleRefreshInterval(timeout, minInterval time.Duration) time.Duration {
	return min(max(timeout/idleRefreshDivisor, minInterval), maxIdleRefreshInterval)
}

// idleDeadlineExtension returns how far past a refresh the deadline is set.
func idleDeadlineExtension(timeout, interval time.Duration) time.Duration {
	return timeout + idleDeadlineSlackIntervals*interval
}
