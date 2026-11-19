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
	waitForWaiters(t, cache, requests+1)
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

// TestUnitCacheStopsAbandonedRun: when every request waiting for a run gave
// up, the run is stopped, and the next request starts a new one instead of
// joining the stopped one.
func TestUnitCacheStopsAbandonedRun(t *testing.T) {
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
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned run was not stopped")
	}
	close(lister.gate)
	if units, _ := mustListUnits(t, cache, 10); !slices.Equal(units, []string{"a.service"}) {
		t.Fatalf("units %q after an abandoned run", units)
	}
	if runs := lister.runs.Load(); runs != 2 {
		t.Fatalf("%d runs, want 2", runs)
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

// TestUnitCacheCapsHungRun: requests that keep arriving, each giving up
// once the next one waits, keep the waiter count above zero; they still do
// not keep a hung run alive beyond the flight timeout.
func TestUnitCacheCapsHungRun(t *testing.T) {
	stopped := make(chan struct{})
	var runs atomic.Int32
	list := func(ctx context.Context, _ int) ([]string, bool, error) {
		<-ctx.Done()
		if runs.Add(1) == 1 {
			close(stopped)
		}
		return nil, false, ctx.Err()
	}
	cache := NewUnitCache(list, time.Minute)
	cache.flightTimeout = 300 * time.Millisecond
	var wg sync.WaitGroup
	join := func() context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		wg.Go(func() { _, _, _ = cache.ListUnits(ctx, 10) })
		return cancel
	}
	previous := join()
	waitForWaiters(t, cache, 1)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-stopped:
			previous()
			wg.Wait()
			return
		case <-deadline:
			previous()
			wg.Wait()
			t.Fatal("hung run was not stopped although its waiters overlapped")
		case <-time.After(20 * time.Millisecond):
		}
		next := join()
		if !waitForFlightWaiters(cache, 2, stopped) {
			next()
			continue
		}
		previous()
		previous = next
	}
}

// waitForFlightWaiters waits until the running flight has n waiting
// requests and reports false when stopped is closed first.
func waitForFlightWaiters(cache *UnitCache, n int, stopped <-chan struct{}) bool {
	for {
		cache.mu.Lock()
		waiters := 0
		if cache.flight != nil {
			waiters = cache.flight.waiters
		}
		cache.mu.Unlock()
		if waiters == n {
			return true
		}
		select {
		case <-stopped:
			return false
		case <-time.After(time.Millisecond):
		}
	}
}

// waitForWaiters waits until the running flight has n waiting requests.
func waitForWaiters(t *testing.T, cache *UnitCache, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cache.mu.Lock()
		waiters := 0
		if cache.flight != nil {
			waiters = cache.flight.waiters
		}
		cache.mu.Unlock()
		if waiters == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests wait for the run, want %d", waiters, n)
		}
		time.Sleep(time.Millisecond)
	}
}
