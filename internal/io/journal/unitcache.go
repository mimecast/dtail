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
// not bound it, and it goes on when all of them gave up: only this timeout
// stops a hung journalctl.
const unitFlightTimeout = 30 * time.Second

// ErrUnitCacheClosed is the error of a listing through a closed UnitCache.
var ErrUnitCacheClosed = errors.New("journal unit cache closed")

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
//     started it: a request that gives up (its context ended) fails alone,
//     and the run goes on also when no request waits for it any more, up
//     to unitFlightTimeout or Close, and its result is cached. It used to be
//     stopped then, so on a host whose journal takes longer to list than
//     one probe waits (10 s) no listing was ever cached: every Tab ran
//     journalctl again, to no avail. A run without a waiter costs at most
//     one journalctl of unitFlightTimeout at a time, as a request arriving
//     meanwhile joins it. A panic of the lister is recovered and is the
//     run's error (errUnitListerPanic), so it fails the waiting requests,
//     not dserver.
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

	// base is the context every run derives from; Close cancels it, so
	// that a run nobody waits for does not outlive dserver's shutdown by
	// up to flightTimeout.
	base   context.Context
	cancel context.CancelFunc

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
	// cancel releases the run's context once it ended.
	cancel context.CancelFunc
}

// NewUnitCache returns a cache of the units list lists, each list reused
// for ttl.
func NewUnitCache(list UnitLister, ttl time.Duration) *UnitCache {
	base, cancel := context.WithCancel(context.Background())
	return &UnitCache{list: list, ttl: ttl, now: time.Now, flightTimeout: unitFlightTimeout,
		base: base, cancel: cancel}
}

// Close stops the run in flight, also one no request waits for, and makes
// every later ListUnits fail with ErrUnitCacheClosed without running the
// lister. dserver calls it when it shuts down. It may be called more than
// once, and on a nil cache.
func (c *UnitCache) Close() {
	if c != nil {
		c.cancel()
	}
}

// ListUnits returns the cached unit list, the result of the running list,
// or of a new one (see UnitCache). It has the signature of ListUnits.
func (c *UnitCache) ListUnits(ctx context.Context, maxUnits int) ([]string, bool, error) {
	if c.base.Err() != nil {
		return nil, false, ErrUnitCacheClosed
	}
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
		// The run goes on for the requests after this one (UnitCache).
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
		return nil, flight
	}
	ctx, cancel := context.WithTimeout(c.base, c.flightTimeout)
	flight := &unitFlight{maxUnits: maxUnits, done: make(chan struct{}), cancel: cancel}
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
