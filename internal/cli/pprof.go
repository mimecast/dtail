package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/dlog"
)

// Mutex and block profiling rates. The /debug/pprof/mutex and
// /debug/pprof/block endpoints return empty profiles unless these runtime
// rates are enabled first; they are off by default in the Go runtime.
//
// The values below were validated during task ss0: a mutex profile fraction of
// 5 (report ~1/5 of contention events) and a block profile rate of 100000ns
// (record blocking events longer than 100µs) were sufficient to measure
// outputManager mutex contention — 12.4ms total delay across three concurrent
// 100MB server-mode dcats, i.e. NOT a bottleneck; an idle follow session
// costs 0 CPU ticks/10s because the 1ms read poll only runs while direct output is
// active and the EOF-ack drops it back to 1s polling. Documented here so the
// locking design is not re-litigated.
const (
	mutexProfileFraction   = 5
	blockProfileRateNanos  = 100000
	pprofReadHeaderTimeout = 5 * time.Second
)

// PProfServer owns a dedicated pprof HTTP server lifecycle.
type PProfServer struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
}

type contextListener func(context.Context, string, string) (net.Listener, error)

// NewPProfServer creates a pprof HTTP server bound to address.
func NewPProfServer(ctx context.Context, address string) (*PProfServer, error) {
	var listenConfig net.ListenConfig
	return newPProfServer(ctx, address, listenConfig.Listen)
}

func newPProfServer(ctx context.Context, address string, listen contextListener) (*PProfServer, error) {
	if ctx == nil {
		return nil, errors.New("create pprof server: context must not be nil")
	}
	listener, err := listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for pprof: %w", err)
	}

	return &PProfServer{
		listener: listener,
		server: &http.Server{
			Handler:           newPProfServeMux(),
			ReadHeaderTimeout: pprofReadHeaderTimeout,
		},
		done: make(chan struct{}),
	}, nil
}

// EnableProfilingRates turns on mutex and block profiling collection so that
// the /debug/pprof/mutex and /debug/pprof/block endpoints actually contain
// samples. Without this the Go runtime keeps both rates at zero and those
// endpoints report empty profiles. Only call this when pprof is enabled so it
// costs nothing in the common (no --pprof) case.
func EnableProfilingRates() {
	runtime.SetMutexProfileFraction(mutexProfileFraction)
	runtime.SetBlockProfileRate(blockProfileRateNanos)
}

// Address returns the bound pprof listener address.
func (s *PProfServer) Address() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Start serves the pprof HTTP endpoints until shutdown.
func (s *PProfServer) Start(wg *sync.WaitGroup) {
	if s == nil {
		return
	}

	if wg != nil {
		wg.Add(1)
	}

	go func() {
		if wg != nil {
			defer wg.Done()
		}
		defer close(s.done)

		if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			dlog.Client.Error("PProf server exited", err)
		}
	}()
}

// Shutdown stops the pprof HTTP server and waits for Serve to return.
func (s *PProfServer) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("shutdown pprof server: context must not be nil")
	}

	err := s.server.Shutdown(ctx)
	select {
	case <-s.done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

func newPProfServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}

	return mux
}
