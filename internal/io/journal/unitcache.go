package journal

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// DefaultUnitCacheTTL is how long dserver reuses a listed set of journal
// units for the path listings of all its sessions. Listing them runs
// journalctl --field _SYSTEMD_UNIT, which reads the whole journal: about
// 2 s of CPU and 560 MB RSS on a 3.4 GB journal. A unit that appears later
// is offered after at most this time.
const DefaultUnitCacheTTL = 60 * time.Second

// unitFlightTimeout bounds one shared run of the unit lister. The run
// belongs to no single request, so the probe timeout of the requests does
// not bound it: requests arriving one after another, each giving up after
// its own timeout, would otherwise keep a hung journalctl alive for as long
// as they keep coming.
const unitFlightTimeout = 30 * time.Second

// errUnitListerPanic reports a unit lister that panicked; the error carries
// the panic value and the stack, which the probe logs.
var errUnitListerPanic = errors.New("journal unit lister panicked")

// UnitLister lists the systemd units of the journal, at most maxUnits of
// them, and reports whether there were more (ListUnits).
type UnitLister func(ctx context.Context, maxUnits int) ([]string, bool, error)

// UnitCache shares the journal unit list between the path listings of all
// sessions of a dserver:
//   - A successful list is reused for its TTL by every request with the
//     same maxUnits; failures are not cached, the next request runs again.
//   - Concurrent requests share one run (single flight) instead of each
//     starting journalctl. The run does not belong to the request that
//     started it: it ends early when every request waiting for it gave up
//     (its context ended), so one session's timeout does not fail the
//     others, and a run nobody waits for any more is stopped. It ends at
//     the latest after unitFlightTimeout. A panic of the lister is
//     recovered and is the run's error (errUnitListerPanic), so it fails
//     the waiting requests, not dserver.
//   - A request with another maxUnits than the cached list starts a shared
//     run of its own, whose list then replaces the cached one; only while
//     a run of another maxUnits is in flight does it run on its own,
//     unshared and uncached (dserver uses one cap, so neither happens
//     there).
//
// It holds one list of at most maxUnits names, so its memory is bounded.
// The units are the same for every user: each listing still filters them
// by its own session user's read permissions. The returned slice is shared
// and must not be modified.
type UnitCache struct {
	list UnitLister
	ttl  time.Duration
	now  func() time.Time
	// flightTimeout bounds a run (unitFlightTimeout; tests lower it).
	flightTimeout time.Duration

	mu     sync.Mutex
	cached *cachedUnits
	flight *unitFlight
}

// cachedUnits is the last successful list.
type cachedUnits struct {
	maxUnits  int
	units     []string
	truncated bool
	listed    time.Time
}

// unitFlight is one running list shared by the requests waiting for it.
type unitFlight struct {
	maxUnits int
	// done is closed once the result fields are set.
	done      chan struct{}
	units     []string
	truncated bool
	err       error
	// waiters counts the requests waiting for done (guarded by
	// UnitCache.mu); cancel stops the run when the last one gave up.
	waiters int
	cancel  context.CancelFunc
}

// NewUnitCache returns a cache of the units list lists, each list reused
// for ttl.
func NewUnitCache(list UnitLister, ttl time.Duration) *UnitCache {
	return &UnitCache{list: list, ttl: ttl, now: time.Now, flightTimeout: unitFlightTimeout}
}

// ListUnits returns the cached unit list, the result of the running list,
// or of a new one (see UnitCache). It has the signature of ListUnits.
func (c *UnitCache) ListUnits(ctx context.Context, maxUnits int) ([]string, bool, error) {
	cached, flight := c.join(maxUnits)
	switch {
	case cached != nil:
		return cached.units, cached.truncated, nil
	case flight == nil:
		return c.list(ctx, maxUnits)
	}
	select {
	case <-flight.done:
		return flight.units, flight.truncated, flight.err
	case <-ctx.Done():
		c.leave(flight)
		return nil, false, fmt.Errorf("list journal units: %w", ctx.Err())
	}
}

// join returns the cached list when it is fresh and of maxUnits, else the
// flight of maxUnits to wait for, started if none runs, or neither when a
// flight of another maxUnits runs.
func (c *UnitCache) join(maxUnits int) (*cachedUnits, *unitFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached := c.cached; cached != nil && cached.maxUnits == maxUnits &&
		c.now().Sub(cached.listed) < c.ttl {
		return cached, nil
	}
	if flight := c.flight; flight != nil {
		if flight.maxUnits != maxUnits {
			return nil, nil
		}
		flight.waiters++
		return nil, flight
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.flightTimeout)
	flight := &unitFlight{maxUnits: maxUnits, done: make(chan struct{}), waiters: 1, cancel: cancel}
	c.flight = flight
	go c.run(ctx, flight)
	return nil, flight
}

// run lists the units for flight and caches a successful result.
func (c *UnitCache) run(ctx context.Context, flight *unitFlight) {
	defer flight.cancel()
	units, truncated, err := c.listRecovered(ctx, flight.maxUnits)
	c.mu.Lock()
	flight.units, flight.truncated, flight.err = units, truncated, err
	if c.flight == flight {
		c.flight = nil
	}
	if err == nil {
		c.cached = &cachedUnits{maxUnits: flight.maxUnits, units: units, truncated: truncated,
			listed: c.now()}
	}
	c.mu.Unlock()
	close(flight.done)
}

// listRecovered runs the lister and turns a panic of it into an error with
// the panic value and stack: the run's goroutine belongs to no session,
// whose panic recovery would otherwise contain it, so an unrecovered panic
// would end dserver. The probe logs the error at WARN.
func (c *UnitCache) listRecovered(ctx context.Context, maxUnits int) (units []string,
	truncated bool, err error) {

	defer func() {
		if recovered := recover(); recovered != nil {
			units, truncated = nil, false
			err = fmt.Errorf("%w: %v\n%s", errUnitListerPanic, recovered, debug.Stack())
		}
	}()
	return c.list(ctx, maxUnits)
}

// leave removes a request that gave up from flight; the last one stops the
// run and lets the next request start a new one instead of joining it.
func (c *UnitCache) leave(flight *unitFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flight.waiters--
	if flight.waiters > 0 {
		return
	}
	flight.cancel()
	if c.flight == flight {
		c.flight = nil
	}
}
