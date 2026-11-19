package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/server"
)

type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

type lifecyclePProfServer struct {
	recorder    *eventRecorder
	shutdownErr error
	hasDeadline bool
	deadline    time.Time
}

type dserverServiceStub struct {
	start func(context.Context) (int, error)
}

type backgroundJobsStub struct{}

func (*backgroundJobsStub) Start(context.Context) {}

func TestRunDServerLifecycleCleansUpConstructionFailureInOrder(t *testing.T) {
	ctx, baseCancel := context.WithCancel(context.Background())
	t.Cleanup(baseCancel)
	recorder := &eventRecorder{}
	var wg sync.WaitGroup
	wg.Add(1)
	go recordLoggerStop(ctx, &wg, recorder)

	profile := &lifecyclePProfServer{recorder: recorder}
	backgroundJobs := &backgroundJobsStub{}
	constructionErr := errors.New("server construction failed")
	var stderr bytes.Buffer
	var notifiedSignals []os.Signal
	status := runDServerLifecycle(ctx, func() {
		recorder.add("logger cancel")
		baseCancel()
	}, &wg, 0, "test-profile", config.RuntimeConfig{}, dserverLifecycleDependencies{
		stderr:  &stderr,
		loggers: nopLoggerDependencies(),
		enableProfilingRates: func() {
			recorder.add("profiling enabled")
		},
		newPProfServer: func(_ context.Context, address string) (profileServer, error) {
			recorder.add("pprof created: " + address)
			return profile, nil
		},
		newBackgroundJobs: func(config.RuntimeConfig, clients.LoggerDependencies) server.BackgroundJobs {
			recorder.add("background jobs construction")
			return backgroundJobs
		},
		newServer: func(_ config.RuntimeConfig, loggers handlers.HandlerLoggers,
			gotBackgroundJobs server.BackgroundJobs) (dserverService, error) {
			if gotBackgroundJobs != backgroundJobs {
				t.Fatalf("background jobs = %T(%p), want %T(%p)",
					gotBackgroundJobs, gotBackgroundJobs, backgroundJobs, backgroundJobs)
			}
			if loggers.Diagnostics == nil || loggers.Reader == nil {
				t.Fatal("server handler logger roles were not injected")
			}
			recorder.add("server construction")
			return nil, constructionErr
		},
		notifyContext: func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
			notifiedSignals = append(notifiedSignals, signals...)
			recorder.add("signals registered")
			signalCtx, signalCancel := context.WithCancel(parent)
			return signalCtx, func() {
				recorder.add("signals stopped")
				signalCancel()
			}
		},
	})

	if status != 1 {
		t.Fatalf("status = %d, want 1", status)
	}
	if !strings.Contains(stderr.String(), constructionErr.Error()) {
		t.Fatalf("stderr = %q, want construction error", stderr.String())
	}
	if len(notifiedSignals) != 2 || notifiedSignals[0] != os.Interrupt || notifiedSignals[1] != syscall.SIGTERM {
		t.Fatalf("notified signals = %v, want interrupt and SIGTERM", notifiedSignals)
	}
	if !profile.hasDeadline {
		t.Fatal("pprof shutdown context has no deadline")
	}
	remaining := time.Until(profile.deadline)
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("pprof shutdown deadline remaining = %v, want within 5s", remaining)
	}
	wantEvents := []string{
		"profiling enabled",
		"pprof created: test-profile",
		"pprof started",
		"signals registered",
		"background jobs construction",
		"server construction",
		"signals stopped",
		"pprof shutdown",
		"logger cancel",
		"logger stopped",
	}
	if got := recorder.snapshot(); !slices.Equal(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestRunDServerLifecycleBuildsAndStopsTimeoutContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	var serverCtx context.Context
	stopCalls := 0

	status := runDServerLifecycle(ctx, cancel, &wg, 30, "", config.RuntimeConfig{},
		dserverLifecycleDependencies{
			stderr:  &bytes.Buffer{},
			loggers: nopLoggerDependencies(),
			newServer: func(config.RuntimeConfig, handlers.HandlerLoggers, server.BackgroundJobs) (dserverService, error) {
				return dserverServiceStub{start: func(ctx context.Context) (int, error) {
					serverCtx = ctx
					return 7, nil
				}}, nil
			},
			notifyContext: func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
				signalCtx, signalCancel := context.WithCancel(parent)
				return signalCtx, func() {
					stopCalls++
					signalCancel()
				}
			},
		})

	if status != 7 {
		t.Fatalf("status = %d, want 7", status)
	}
	if stopCalls != 1 {
		t.Fatalf("signal stop calls = %d, want 1", stopCalls)
	}
	if serverCtx == nil {
		t.Fatal("server Start did not receive a context")
	}
	deadline, ok := serverCtx.Deadline()
	if !ok {
		t.Fatal("server context has no shutdown deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("server deadline remaining = %v, want within 30s", remaining)
	}
	if !errors.Is(serverCtx.Err(), context.Canceled) {
		t.Fatalf("server context error after return = %v, want canceled", serverCtx.Err())
	}
}

func (r *eventRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *eventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (s *lifecyclePProfServer) Address() string {
	return "test-profile"
}

func (s *lifecyclePProfServer) Start(*sync.WaitGroup) {
	s.recorder.add("pprof started")
}

func (s *lifecyclePProfServer) Shutdown(ctx context.Context) error {
	s.deadline, s.hasDeadline = ctx.Deadline()
	s.recorder.add("pprof shutdown")
	return s.shutdownErr
}

func (s dserverServiceStub) Start(ctx context.Context) (int, error) {
	return s.start(ctx)
}

func recordLoggerStop(ctx context.Context, wg *sync.WaitGroup, recorder *eventRecorder) {
	defer wg.Done()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		recorder.add("logger stopped")
	case <-timer.C:
		recorder.add("logger stop timed out")
	}
}

func nopLoggerDependencies() clients.LoggerDependencies {
	return clients.NewLoggerDependencies(clientlog.NopLogger{}, logging.NopLogger{}, logging.NopLogger{})
}
