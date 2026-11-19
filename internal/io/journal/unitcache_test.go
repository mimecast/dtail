package journal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUnitLister counts its runs and, when gate is set, holds every run
// until gate is closed or its context ends.
type fakeUnitLister struct {
	runs    atomic.Int32
	started chan struct{}
	gate    chan struct{}
	stopped chan struct{}
	units   []string
	err     error
}

func newFakeUnitLister(units ...string) *fakeUnitLister {
	return &fakeUnitLister{started: make(chan struct{}, 16), stopped: make(chan struct{}, 16), units: units}
}

func (f *fakeUnitLister) list(ctx context.Context, maxUnits int) ([]string, bool, error) {
	f.runs.Add(1)
	f.started <- struct{}{}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			f.stopped <- struct{}{}
			return nil, false, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, false, f.err
	}
	units := f.units
	if len(units) > maxUnits {
		return units[:maxUnits], true, nil
	}
	return units, false, nil
}

// fakeClock is a settable clock for the cache's TTL.
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

func newTestUnitCache(lister *fakeUnitLister) (*UnitCache, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	cache := NewUnitCache(lister.list, time.Minute)
	cache.now = clock.Now
	return cache, clock
}

func mustListUnits(t *testing.T, cache *UnitCache, maxUnits int) ([]string, bool) {
	t.Helper()
	units, truncated, err := cache.ListUnits(context.Background(), maxUnits)
	if err != nil {
		t.Fatalf("ListUnits: %v", err)
	}
	return units, truncated
}

// TestUnitCacheReusesListWithinTTL: a list is reused until its TTL ended,
// then listed again; a request with another cap lists on its own.
func TestUnitCacheReusesListWithinTTL(t *testing.T) {
	lister := newFakeUnitLister("a.service", "b.service")
	cache, clock := newTestUnitCache(lister)
	for range 3 {
		if units, truncated := mustListUnits(t, cache, 10); !slices.Equal(units, lister.units) || truncated {
			t.Fatalf("units %q truncated %v", units, truncated)
		}
	}
	clock.advance(59 * time.Second)
	mustListUnits(t, cache, 10)
	if runs := lister.runs.Load(); runs != 1 {
		t.Fatalf("%d runs within the TTL, want 1", runs)
	}
	clock.advance(time.Second)
	mustListUnits(t, cache, 10)
	if runs := lister.runs.Load(); runs != 2 {
		t.Fatalf("%d runs after the TTL, want 2", runs)
	}
	if units, truncated := mustListUnits(t, cache, 1); !slices.Equal(units, []string{"a.service"}) || !truncated {
		t.Fatalf("cap 1: units %q truncated %v", units, truncated)
	}
	if runs := lister.runs.Load(); runs != 3 {
		t.Fatalf("%d runs after a request with another cap, want 3", runs)
	}
}

// TestUnitCacheDoesNotCacheFailures: a failed list is reported to its
// requests and the next request lists again.
func TestUnitCacheDoesNotCacheFailures(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.err = errors.New("journalctl failed")
	cache, _ := newTestUnitCache(lister)
	if _, _, err := cache.ListUnits(context.Background(), 10); !errors.Is(err, lister.err) {
		t.Fatalf("ListUnits error %v, want %v", err, lister.err)
	}
	lister.err = nil
	if units, _ := mustListUnits(t, cache, 10); !slices.Equal(units, []string{"a.service"}) {
		t.Fatalf("units %q after a failure", units)
	}
	if runs := lister.runs.Load(); runs != 2 {
		t.Fatalf("%d runs, want 2 (the failure is not cached)", runs)
	}
}

// TestUnitCacheSharesConcurrentRun: requests arriving while a list runs
// wait for it instead of starting their own, and one of them giving up
// neither stops the run nor fails the others.
func TestUnitCacheSharesConcurrentRun(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.gate = make(chan struct{})
	cache, _ := newTestUnitCache(lister)
	const requests = 5
	errs := make(chan error, requests)
	for range requests {
		go func() {
			units, _, err := cache.ListUnits(context.Background(), 10)
			if err == nil && !slices.Equal(units, []string{"a.service"}) {
				err = errors.New("wrong units")
			}
			errs <- err
		}()
	}
	<-lister.started
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { _, _, err := cache.ListUnits(ctx, 10); gaveUp <- err }()
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v, want context.Canceled", err)
	}
	close(lister.gate)
	for range requests {
		if err := <-errs; err != nil {
			t.Fatalf("request: %v", err)
		}
	}
	if runs := lister.runs.Load(); runs != 1 {
		t.Fatalf("%d runs for %d concurrent requests, want 1", runs, requests+1)
	}
}

// TestUnitCacheAbandonedRunGoesOnAndIsCached: when the only request waiting
// for a run gives up (a slow journal against the 10 s probe timeout), the
// run goes on. A request arriving meanwhile joins it instead of starting
// journalctl again, and its result is cached, so the next request is
// answered without a run. It used to be stopped, so a listing slower than
// one probe's timeout was never cached and every Tab ran journalctl anew.
func TestUnitCacheAbandonedRunGoesOnAndIsCached(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.gate = make(chan struct{})
	cache, _ := newTestUnitCache(lister)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := cache.ListUnits(ctx, 10); done <- err }()
	<-lister.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ListUnits: %v, want context.Canceled", err)
	}
	select {
	case <-lister.stopped:
		t.Fatal("the run was stopped when its only request gave up")
	case <-time.After(50 * time.Millisecond):
	}

	joined := make(chan []string, 1)
	go func() {
		units, _, err := cache.ListUnits(context.Background(), 10)
		if err != nil {
			units = []string{err.Error()}
		}
		joined <- units
	}()
	select {
	case <-lister.started:
		t.Fatal("a request arriving during the abandoned run started a run of its own")
	case <-time.After(50 * time.Millisecond):
	}
	close(lister.gate)
	if units := <-joined; !slices.Equal(units, []string{"a.service"}) {
		t.Fatalf("units %q of the request that joined the abandoned run", units)
	}
	if units, _ := mustListUnits(t, cache, 10); !slices.Equal(units, []string{"a.service"}) {
		t.Fatalf("units %q after an abandoned run", units)
	}
	if runs := lister.runs.Load(); runs != 1 {
		t.Fatalf("%d runs, want 1: the abandoned run's result is cached", runs)
	}
}

// TestUnitCacheAbandonedRunResultIsCachedWithoutWaiter: the result of a run
// nobody waited for at its end is cached all the same.
func TestUnitCacheAbandonedRunResultIsCachedWithoutWaiter(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.gate = make(chan struct{})
	cache, _ := newTestUnitCache(lister)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cache.ListUnits(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListUnits: %v, want context.Canceled", err)
	}
	close(lister.gate)
	waitForNoFlight(t, cache)
	if units, _ := mustListUnits(t, cache, 10); !slices.Equal(units, []string{"a.service"}) {
		t.Fatalf("units %q after an abandoned run", units)
	}
	if runs := lister.runs.Load(); runs != 1 {
		t.Fatalf("%d runs, want 1", runs)
	}
}

// TestUnitCacheAbandonedFailedRunIsNotCached: an abandoned run that fails
// caches nothing (negative case); the next request runs again.
func TestUnitCacheAbandonedFailedRunIsNotCached(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.gate = make(chan struct{})
	lister.err = errors.New("journalctl failed")
	cache, _ := newTestUnitCache(lister)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cache.ListUnits(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListUnits: %v, want context.Canceled", err)
	}
	close(lister.gate)
	waitForNoFlight(t, cache)
	if _, _, err := cache.ListUnits(context.Background(), 10); !errors.Is(err, lister.err) {
		t.Fatalf("ListUnits after a failed abandoned run: %v, want its own failure", err)
	}
	if runs := lister.runs.Load(); runs != 2 {
		t.Fatalf("%d runs, want 2: a failure is not cached", runs)
	}
}

// TestUnitCacheRecoversListerPanic: a panic of the lister in the shared run
// fails the waiting requests with errUnitListerPanic (stack included)
// instead of ending the process, leaves no flight behind and caches
// nothing, so the next request runs again.
func TestUnitCacheRecoversListerPanic(t *testing.T) {
	var runs atomic.Int32
	list := func(context.Context, int) ([]string, bool, error) {
		if runs.Add(1) == 1 {
			panic("lister exploded")
		}
		return []string{"a.service"}, false, nil
	}
	cache := NewUnitCache(list, time.Minute)
	_, _, err := cache.ListUnits(context.Background(), 10)
	if !errors.Is(err, errUnitListerPanic) || !strings.Contains(err.Error(), "lister exploded") ||
		!strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("ListUnits error %v, want the recovered panic with its stack", err)
	}
	cache.mu.Lock()
	flight, cached := cache.flight, cache.cached
	cache.mu.Unlock()
	if flight != nil || cached != nil {
		t.Fatalf("after a panic: flight %v cached %v, want neither", flight, cached)
	}
	if units, _ := mustListUnits(t, cache, 10); !slices.Equal(units, []string{"a.service"}) {
		t.Fatalf("units %q after a panic", units)
	}
}

// TestUnitCacheCapsHungRun: a hung run is stopped at the flight timeout,
// whether a request still waits for it (and then fails) or its only
// request gave up long before, and the next request starts a new run.
func TestUnitCacheCapsHungRun(t *testing.T) {
	for _, abandoned := range []bool{false, true} {
		lister := newFakeUnitLister("a.service")
		lister.gate = make(chan struct{})
		cache, _ := newTestUnitCache(lister)
		cache.flightTimeout = 200 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		if abandoned {
			cancel()
		}
		_, _, err := cache.ListUnits(ctx, 10)
		cancel()
		want := context.DeadlineExceeded
		if abandoned {
			want = context.Canceled
		}
		if !errors.Is(err, want) {
			t.Fatalf("abandoned %v: ListUnits: %v, want %v", abandoned, err, want)
		}
		select {
		case <-lister.stopped:
		case <-time.After(5 * time.Second):
			t.Fatalf("abandoned %v: hung run was not stopped at the flight timeout", abandoned)
		}
		waitForNoFlight(t, cache)
		close(lister.gate)
		if units, _ := mustListUnits(t, cache, 10); !slices.Equal(units, []string{"a.service"}) {
			t.Fatalf("abandoned %v: units %q after a hung run", abandoned, units)
		}
		if runs := lister.runs.Load(); runs != 2 {
			t.Fatalf("abandoned %v: %d runs, want 2", abandoned, runs)
		}
	}
}

// TestUnitCacheCloseStopsAbandonedRun: Close (dserver's shutdown) stops a
// run no request waits for any more, well before the flight timeout,
// caches nothing of it, and a listing afterwards fails with
// ErrUnitCacheClosed without running the lister. Close on a nil cache and
// a second Close do nothing.
func TestUnitCacheCloseStopsAbandonedRun(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.gate = make(chan struct{})
	defer close(lister.gate)
	cache, _ := newTestUnitCache(lister)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cache.ListUnits(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListUnits: %v, want context.Canceled", err)
	}
	<-lister.started
	select {
	case <-lister.stopped:
		t.Fatal("the abandoned run stopped before Close")
	case <-time.After(50 * time.Millisecond):
	}
	cache.Close()
	select {
	case <-lister.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the abandoned run")
	}
	waitForNoFlight(t, cache)
	cache.Close()
	if _, _, err := cache.ListUnits(context.Background(), 10); !errors.Is(err, ErrUnitCacheClosed) {
		t.Fatalf("ListUnits after Close: %v, want ErrUnitCacheClosed", err)
	}
	if runs := lister.runs.Load(); runs != 1 {
		t.Fatalf("%d runs, want 1: a closed cache runs no lister", runs)
	}
	var none *UnitCache
	none.Close()
}

// TestUnitCacheCloseFailsWaitingRequest: a request waiting for the run
// when the cache is closed gets the run's error instead of hanging.
func TestUnitCacheCloseFailsWaitingRequest(t *testing.T) {
	lister := newFakeUnitLister("a.service")
	lister.gate = make(chan struct{})
	defer close(lister.gate)
	cache, _ := newTestUnitCache(lister)
	done := make(chan error, 1)
	go func() { _, _, err := cache.ListUnits(context.Background(), 10); done <- err }()
	<-lister.started
	cache.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting request: %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiting request hangs after Close")
	}
}

// waitForNoFlight waits until no run is in flight.
func waitForNoFlight(t *testing.T, cache *UnitCache) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cache.mu.Lock()
		flight := cache.flight
		cache.mu.Unlock()
		if flight == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a run is still in flight")
		}
		time.Sleep(time.Millisecond)
	}
}
