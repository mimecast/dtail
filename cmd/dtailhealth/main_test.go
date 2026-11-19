package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/cli"
	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
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

type healthClientStub struct {
	status int
	starts int
}

type errorRecordingLogger struct {
	clientlog.NopLogger
	errors []string
}

func TestRunHealthLifecycleReturnsClientStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	client := &healthClientStub{status: 3}

	runtimeCfg := config.RuntimeConfig{Common: &config.CommonConfig{HostnameOverride: "test-host"}}
	status := runHealthLifecycle(ctx, cancel, &wg, config.Args{}, runtimeCfg, "", healthLifecycleDependencies{
		stderr:  &bytes.Buffer{},
		loggers: nopLoggerDependencies(),
		newHealthClient: func(_ config.Args, gotCfg config.RuntimeConfig,
			_ clients.LoggerDependencies) (clients.Client, error) {
			if gotCfg.Common == nil || gotCfg.Common.HostnameOverride != "test-host" {
				t.Fatalf("runtime config = %#v, want injected config", gotCfg)
			}
			return client, nil
		},
	})

	if status != 3 {
		t.Fatalf("status = %d, want 3", status)
	}
	if client.starts != 1 {
		t.Fatalf("client Start calls = %d, want 1", client.starts)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("lifecycle context error = %v, want canceled", ctx.Err())
	}
}

func TestRunHealthLifecycleCleansUpConstructionFailureInOrder(t *testing.T) {
	ctx, baseCancel := context.WithCancel(context.Background())
	t.Cleanup(baseCancel)
	recorder := &eventRecorder{}
	var wg sync.WaitGroup
	wg.Add(1)
	go recordLoggerStop(ctx, &wg, recorder)

	shutdownErr := errors.New("pprof shutdown failed")
	profile := &lifecyclePProfServer{recorder: recorder, shutdownErr: shutdownErr}
	logger := &errorRecordingLogger{}
	loggers := clients.NewLoggerDependencies(logger, logging.NopLogger{}, logging.NopLogger{})
	constructionErr := errors.New("health client construction failed")
	var stderr bytes.Buffer
	status := runHealthLifecycle(ctx, func() {
		recorder.add("logger cancel")
		baseCancel()
	}, &wg, config.Args{}, config.RuntimeConfig{}, "test-profile", healthLifecycleDependencies{
		stderr:  &stderr,
		loggers: loggers,
		newPProfServer: func(_ context.Context, address string) (profileServer, error) {
			recorder.add("pprof created: " + address)
			return profile, nil
		},
		newHealthClient: func(config.Args, config.RuntimeConfig,
			clients.LoggerDependencies) (clients.Client, error) {
			recorder.add("health client construction")
			return nil, constructionErr
		},
	})

	if status != 2 {
		t.Fatalf("status = %d, want 2", status)
	}
	if !strings.Contains(stderr.String(), constructionErr.Error()) {
		t.Fatalf("stderr = %q, want construction error", stderr.String())
	}
	if !profile.hasDeadline {
		t.Fatal("pprof shutdown context has no deadline")
	}
	remaining := time.Until(profile.deadline)
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("pprof shutdown deadline remaining = %v, want within 5s", remaining)
	}
	if len(logger.errors) != 1 || !strings.Contains(logger.errors[0], shutdownErr.Error()) {
		t.Fatalf("logged errors = %q, want pprof shutdown failure", logger.errors)
	}
	wantEvents := []string{
		"pprof created: test-profile",
		"pprof started",
		"health client construction",
		"pprof shutdown",
		"logger cancel",
		"logger stopped",
	}
	if got := recorder.snapshot(); !slices.Equal(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
}

func TestShutdownPProfStopsServer(t *testing.T) {
	profile, err := cli.NewPProfServer(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewPProfServer: %v", err)
	}
	profile.Start(nil)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			shutdownPProf(context.Background(), profile, logging.NopLogger{})
		}
	})

	client := &http.Client{
		Timeout:   time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	t.Cleanup(client.CloseIdleConnections)
	url := "http://" + profile.Address() + "/debug/pprof/"
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET running pprof server: %v", err)
	}
	statusCode := response.StatusCode
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatalf("close pprof response: %v", closeErr)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("pprof status = %d, want %d", statusCode, http.StatusOK)
	}

	shutdownPProf(context.Background(), profile, logging.NopLogger{})
	stopped = true

	response, err = client.Get(url)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("GET stopped pprof server unexpectedly succeeded")
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

func (c *healthClientStub) Start(context.Context, <-chan string) int {
	c.starts++
	return c.status
}

func (l *errorRecordingLogger) Error(args ...any) string {
	message := fmt.Sprint(args...)
	l.errors = append(l.errors, message)
	return message
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
