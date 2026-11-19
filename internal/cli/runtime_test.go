package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/profiling"
	"github.com/mimecast/dtail/internal/source"
)

func TestNewClientRuntimeReturnsLoggerStartError(t *testing.T) {
	var loggerCtx context.Context
	runtime, err := newClientRuntime(context.Background(), profiling.Flags{}, "test",
		func(ctx context.Context, wg *sync.WaitGroup, process source.Source) error {
			loggerCtx = ctx
			return errors.New("logger setup failed")
		})
	if runtime != nil {
		t.Fatalf("runtime = %#v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), "logger setup failed") {
		t.Fatalf("newClientRuntime error = %v, want wrapped logger failure", err)
	}
	if loggerCtx == nil || loggerCtx.Err() == nil {
		t.Fatal("logger context was not canceled after startup failure")
	}
}

func TestNewClientRuntimeRejectsNilContext(t *testing.T) {
	started := false
	var nilContext context.Context
	runtime, err := newClientRuntime(nilContext, profiling.Flags{}, "test",
		func(context.Context, *sync.WaitGroup, source.Source) error {
			started = true
			return nil
		})
	if runtime != nil || err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("newClientRuntime(nil) = (%v, %v), want nil runtime and context error", runtime, err)
	}
	if started {
		t.Fatal("logger started before nil context was rejected")
	}
}

func TestNewClientRuntimeStopsEnabledProfilerWhenLoggerStartFails(t *testing.T) {
	profiler := &recordingClientProfiler{}
	runtime, err := newClientRuntimeWithProfiler(
		context.Background(),
		profiling.Flags{MemProfile: true},
		"test",
		func(context.Context, *sync.WaitGroup, source.Source) error {
			return errors.New("logger setup failed")
		},
		func(cfg profiling.Config) clientProfiler {
			if !cfg.MemProfile {
				t.Fatal("profiler factory received disabled memory profile")
			}
			return profiler
		},
	)
	if runtime != nil {
		t.Fatalf("runtime = %#v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), "logger setup failed") {
		t.Fatalf("newClientRuntime error = %v, want wrapped logger failure", err)
	}
	if profiler.stopCalls != 1 {
		t.Fatalf("profiler Stop calls = %d, want 1", profiler.stopCalls)
	}
}

type recordingClientProfiler struct {
	stopCalls int
}

func (*recordingClientProfiler) LogMetrics(string) {}
func (p *recordingClientProfiler) Stop()           { p.stopCalls++ }

func TestClientRuntimeKeepsLoggerAliveUntilStop(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	loggerCtxCh := make(chan context.Context, 1)
	loggerStopped := make(chan struct{})
	loggerMessages := make(chan string, 1)
	var logged string
	startLogger := func(ctx context.Context, wg *sync.WaitGroup, process source.Source) error {
		if process != source.Client {
			t.Errorf("logger process = %v, want client", process)
		}
		loggerCtxCh <- ctx
		go func() {
			defer wg.Done()
			defer close(loggerStopped)
			for {
				select {
				case message := <-loggerMessages:
					logged += message
				case <-ctx.Done():
					for {
						select {
						case message := <-loggerMessages:
							logged += message
						default:
							return
						}
					}
				}
			}
		}()
		return nil
	}
	runtime, err := newClientRuntime(parent, profiling.Flags{}, "test", startLogger)
	if err != nil {
		t.Fatalf("new client runtime: %v", err)
	}
	loggerCtx := <-loggerCtxCh

	cancelParent()
	<-runtime.Context().Done()
	if err := loggerCtx.Err(); err != nil {
		t.Fatalf("logger stopped with work context: %v", err)
	}

	loggerMessages <- "final aggregate"
	runtime.Stop()
	select {
	case <-loggerStopped:
	default:
		t.Fatal("runtime Stop returned before logger shutdown completed")
	}
	if logged != "final aggregate" {
		t.Fatalf("post-cancellation logger output = %q, want final aggregate", logged)
	}
}

func TestClientRuntimeStopShutsDownPProf(t *testing.T) {
	prevClient := dlog.Client
	dlog.Client = &dlog.DLog{}
	t.Cleanup(func() {
		dlog.Client = prevClient
	})

	ctx, cancel := context.WithCancel(context.Background())
	runtime := &ClientRuntime{
		ctx:      ctx,
		cancel:   cancel,
		profiler: profiling.NewProfiler(profiling.Config{}),
	}

	runtime.StartPProf("127.0.0.1:0")
	if runtime.pprofServer == nil {
		t.Fatal("expected pprof server to start")
	}

	url := "http://" + runtime.pprofServer.Address() + "/debug/pprof/"
	waitForHTTPStatus(t, url, http.StatusOK)

	runtime.Stop()
	waitForHTTPError(t, url)
}

func waitForHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := http.Get(url)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatalf("close response body: %v", closeErr)
			}
			if resp.StatusCode == want {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s to return %d", url, want)
		}
	}
}

func waitForHTTPError(t *testing.T, url string) {
	t.Helper()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := http.Get(url)
		if err != nil {
			return
		}
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Fatalf("close response body: %v", closeErr)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s to stop serving", url)
		}
	}
}
