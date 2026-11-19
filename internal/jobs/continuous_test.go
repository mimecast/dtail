package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

type immediateBackgroundClient struct{}

func TestSameCalendarDay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    time.Time
		b    time.Time
		want bool
	}{
		{
			name: "same day",
			a:    time.Date(2026, time.January, 15, 10, 0, 0, 0, time.UTC),
			b:    time.Date(2026, time.January, 15, 23, 59, 59, 0, time.UTC),
			want: true,
		},
		{
			name: "same day-of-month in different months",
			a:    time.Date(2026, time.January, 15, 10, 0, 0, 0, time.UTC),
			b:    time.Date(2026, time.February, 15, 10, 0, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "same day-of-month across years",
			a:    time.Date(2025, time.December, 31, 10, 0, 0, 0, time.UTC),
			b:    time.Date(2026, time.January, 31, 10, 0, 0, 0, time.UTC),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sameCalendarDay(tt.a, tt.b); got != tt.want {
				t.Fatalf("sameCalendarDay(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestContinuousRunJobDisablesAuthKeyRegistration(t *testing.T) {

	c := newContinuous(config.RuntimeConfig{
		Server: &config.ServerConfig{SSHBindAddress: "127.0.0.1"},
	}, jobTestLoggers)
	var capturedArgs config.Args
	c.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		capturedArgs = args
		if mode != clients.NonCumulativeMode {
			t.Fatalf("Unexpected client mode: %v", mode)
		}
		return immediateBackgroundClient{}, nil
	}

	job := config.Continuous{}
	job.Name = "continuous-job"
	job.Query = "select count(*)"
	c.runJob(context.Background(), &job)

	if !capturedArgs.NoAuthKey {
		t.Fatal("Expected continuous client to disable AUTHKEY registration")
	}
	if capturedArgs.UserName != config.ContinuousUser {
		t.Fatalf("Unexpected user name: %q", capturedArgs.UserName)
	}
}

func TestContinuousRunJobsReleasesDayChangeWatcherAcrossRetries(t *testing.T) {

	c := newContinuous(config.RuntimeConfig{
		Server: &config.ServerConfig{
			SSHBindAddress: "127.0.0.1",
		},
	}, jobTestLoggers)
	c.retryInterval = 25 * time.Millisecond

	var watcherStarts int32
	var watcherExits int32
	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	c.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		return blockingContinuousClient{
			started: started,
			release: release,
		}, nil
	}
	c.dayChangeWatcher = func(ctx context.Context) bool {
		atomic.AddInt32(&watcherStarts, 1)
		defer atomic.AddInt32(&watcherExits, 1)
		return c.waitForDayChange(ctx)
	}

	job := config.Continuous{}
	job.Enable = true
	job.RestartOnDayChange = true
	c.cfg.Server.Continuous = []config.Continuous{job}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.runJobs(ctx)
		close(done)
	}()

	for i := int32(1); i <= 5; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for retry %d to start", i)
		}

		waitForCounterAtLeast(t, func() int32 {
			return atomic.LoadInt32(&watcherStarts)
		}, i)

		release <- struct{}{}

		waitForCounterAtLeast(t, func() int32 {
			return atomic.LoadInt32(&watcherExits)
		}, i)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("continuous job runner did not stop after cancellation")
	}
	if starts, exits := atomic.LoadInt32(&watcherStarts), atomic.LoadInt32(&watcherExits); starts != exits {
		t.Fatalf("day-change watchers still active after runJobs returned: starts=%d exits=%d", starts, exits)
	}
}

func TestContinuousRunJobsReturnsWhenNoJobsAreEnabled(t *testing.T) {
	disabledJob := config.Continuous{}
	disabledJob.Enable = false
	c := newContinuous(config.RuntimeConfig{Server: &config.ServerConfig{
		Continuous: []config.Continuous{disabledJob},
	}}, jobTestLoggers)

	c.runJobs(context.Background())
}

func TestContinuousWaitForDayChangeDetectsMonthBoundary(t *testing.T) {

	c := newContinuous(config.RuntimeConfig{}, jobTestLoggers)

	start := time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC)
	sameDay := time.Date(2026, time.January, 31, 23, 59, 59, 500_000_000, time.UTC)
	nextDay := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)

	var nowCalls int32
	c.now = func() time.Time {
		switch atomic.AddInt32(&nowCalls, 1) {
		case 1:
			return start
		case 2:
			return sameDay
		default:
			return nextDay
		}
	}

	tickCh := make(chan time.Time, 2)
	c.newTicker = func(time.Duration) (<-chan time.Time, func()) {
		return tickCh, func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan bool, 1)
	go func() {
		result <- c.waitForDayChange(ctx)
	}()

	tickCh <- start
	select {
	case got := <-result:
		t.Fatalf("waitForDayChange returned after same-day tick: %v", got)
	case <-time.After(100 * time.Millisecond):
	}

	tickCh <- nextDay
	select {
	case got := <-result:
		if !got {
			t.Fatal("waitForDayChange returned false after the month boundary tick")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for waitForDayChange to detect the month boundary")
	}
}

type blockingContinuousClient struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (immediateBackgroundClient) Start(context.Context, <-chan string) int {
	return 0
}

func (f blockingContinuousClient) Start(ctx context.Context, _ <-chan string) int {
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return 1
	}
	select {
	case <-f.release:
		return 0
	case <-ctx.Done():
		return 1
	}
}

func waitForCounterAtLeast(t *testing.T, current func() int32, minimum int32) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if current() >= minimum {
			return
		}

		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for counter to reach %d, got %d", minimum, current())
		case <-ticker.C:
		}
	}
}
