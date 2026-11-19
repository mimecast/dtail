package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

func TestNewPProfServerSetsReadHeaderTimeout(t *testing.T) {
	server, err := NewPProfServer(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewPProfServer: %v", err)
	}
	defer func() { _ = server.listener.Close() }()

	if got := server.server.ReadHeaderTimeout; got != pprofReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", got, pprofReadHeaderTimeout)
	}
}

func TestNewPProfServerCancellationStopsBlockedListen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := newPProfServer(ctx, "127.0.0.1:0",
			func(ctx context.Context, network, address string) (net.Listener, error) {
				if network != "tcp" || address != "127.0.0.1:0" {
					t.Errorf("listen target = %s %s", network, address)
				}
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			})
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked listen error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pprof listen did not stop after context cancellation")
	}
}

func TestNewPProfServerRejectsNilContext(t *testing.T) {
	var nilContext context.Context
	server, err := NewPProfServer(nilContext, "127.0.0.1:0")
	if server != nil || err == nil {
		t.Fatalf("NewPProfServer(nil) = (%v, %v), want nil server and error", server, err)
	}
}

func TestPProfShutdownCancellationStopsBlockedServeWait(t *testing.T) {
	server := &PProfServer{
		server: &http.Server{},
		done:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- server.Shutdown(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Shutdown error = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Shutdown ignored cancellation while Serve completion was blocked")
	}
}

func TestPProfShutdownRejectsNilContext(t *testing.T) {
	server := &PProfServer{server: &http.Server{}, done: make(chan struct{})}
	var nilContext context.Context
	if err := server.Shutdown(nilContext); err == nil {
		t.Fatal("Shutdown(nil) succeeded, want context error")
	}
}

// TestEnableProfilingRatesSetsMutexFraction verifies that EnableProfilingRates
// actually turns on mutex profiling. Without this the /debug/pprof/mutex
// endpoint reports an empty profile. runtime.SetMutexProfileFraction(-1) reads
// the current fraction without changing it, so it lets us assert the state.
func TestEnableProfilingRatesSetsMutexFraction(t *testing.T) {
	// Save and restore global runtime state so this test does not leak into
	// other tests in the package. The block profile rate has no getter, so we
	// simply disable it again on cleanup.
	prevMutex := runtime.SetMutexProfileFraction(-1)
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(prevMutex)
		runtime.SetBlockProfileRate(0)
	})

	// Start from a known-disabled state.
	runtime.SetMutexProfileFraction(0)

	EnableProfilingRates()

	if got := runtime.SetMutexProfileFraction(-1); got != mutexProfileFraction {
		t.Errorf("mutex profile fraction = %d, want %d", got, mutexProfileFraction)
	}
}

// TestProfilingRateConstants pins the ss0-validated values so a change is a
// conscious decision rather than an accident.
func TestProfilingRateConstants(t *testing.T) {
	if mutexProfileFraction != 5 {
		t.Errorf("mutexProfileFraction = %d, want 5", mutexProfileFraction)
	}
	if blockProfileRateNanos != 100000 {
		t.Errorf("blockProfileRateNanos = %d, want 100000", blockProfileRateNanos)
	}
}
