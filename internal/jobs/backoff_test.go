package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

func TestBackoffDelay(t *testing.T) {
	t.Parallel()

	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, minutes := range want {
		if got := backoffDelay(i + 1); got != minutes*time.Minute {
			t.Errorf("backoffDelay(%d) = %v, want %v", i+1, got, minutes*time.Minute)
		}
	}
	if got := backoffDelay(1000); got != time.Hour {
		t.Errorf("backoffDelay(1000) = %v, want 1h", got)
	}
}

// backoffTestScheduler is a scheduler with a fake clock whose single job
// ends with the next of statuses on every run, and records when it ran.
type backoffTestScheduler struct {
	*scheduler
	now      time.Time
	statuses []int
	runs     []time.Time
	// modes are the client modes of the runs.
	modes  []clients.MaprClientMode
	logger *exitLogger
	// runTime is how long every run of the job takes on the fake clock.
	runTime time.Duration
}

// backoffTestClient is a client that ends with status after it advanced the fake
// clock by runTime.
type backoffTestClient struct {
	b      *backoffTestScheduler
	status int
}

func (c backoffTestClient) Start(context.Context, <-chan string) int {
	c.b.now = c.b.now.Add(c.b.runTime)
	return c.status
}

func newBackoffTestScheduler(t *testing.T, outfile string, start time.Time) *backoffTestScheduler {
	t.Helper()
	cfg := config.RuntimeConfig{Server: &config.ServerConfig{SSHBindAddress: "127.0.0.1"}}
	job := config.Scheduled{}
	job.Name = "flaky"
	job.Enable = true
	job.TimeRange = [2]int{0, 24}
	job.Query = "from STATS select count($line)"
	job.Outfile = outfile
	cfg.Server.Schedule = []config.Scheduled{job}

	b := &backoffTestScheduler{scheduler: newScheduler(cfg, jobTestLoggers), now: start, logger: &exitLogger{}}
	b.scheduler.logger = b.logger
	b.scheduler.now = func() time.Time { return b.now }
	b.newMaprClient = func(_ config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		if len(b.statuses) == 0 {
			t.Fatal("job ran more often than expected")
		}
		status := b.statuses[0]
		b.statuses = b.statuses[1:]
		b.runs = append(b.runs, b.now)
		b.modes = append(b.modes, mode)
		return backoffTestClient{b: b, status: status}, nil
	}
	return b
}

// runEveryMinute runs the scheduler every minute, as its ticker does, for
// minutes minutes from the fake clock's time on. A ticker drops the ticks
// that come while a run is still going, so does this.
func (b *backoffTestScheduler) runEveryMinute(minutes int) {
	end := b.now.Add(time.Duration(minutes) * time.Minute)
	for tick := b.now; tick.Before(end); tick = tick.Add(time.Minute) {
		if b.now.After(tick) {
			continue
		}
		b.now = tick
		b.runJobs(context.Background())
	}
}

// minutesOfRuns returns when the job ran, in minutes after start.
func (b *backoffTestScheduler) minutesOfRuns(start time.Time) []int {
	minutes := make([]int, len(b.runs))
	for i, run := range b.runs {
		minutes[i] = int(run.Sub(start) / time.Minute)
	}
	return minutes
}

// TestSchedulerBacksOffAFailingJob runs the scheduler every minute with a
// job that keeps failing: it runs again 1, 2, 4, ... minutes after each
// failed run, and at most every hour.
func TestSchedulerBacksOffAFailingJob(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result.csv"), start)
	b.statuses = slices.Repeat([]int{1}, 9)

	b.runEveryMinute(4 * 60)

	want := []int{0, 1, 3, 7, 15, 31, 63, 123, 183}
	if got := b.minutesOfRuns(start); !slices.Equal(got, want) {
		t.Fatalf("job ran at minutes %v, want %v", got, want)
	}
	wantLog := fmt.Sprintf("Job flaky failed and wrote no outfile %s (failure 3 in a row), it runs again from about %s",
		filepath.Join(filepath.Dir(b.cfg.Server.Schedule[0].Outfile), "result.csv"),
		start.Add(7*time.Minute).Format(time.DateTime))
	if !slices.Contains(b.logger.lines, wantLog) {
		t.Fatalf("log = %q, want %q", b.logger.lines, wantLog)
	}
	// Failures are logged once per run, not once per scheduler run.
	if got := countLines(b.logger.lines, "Job flaky failed and wrote no outfile"); got != len(want) {
		t.Fatalf("logged %d failures, want %d", got, len(want))
	}
}

// TestSchedulerBackoffEndsWhenTheJobSucceeds checks that a job that
// succeeded after failures starts its backoff anew when it fails again.
func TestSchedulerBackoffEndsWhenTheJobSucceeds(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result.csv"), start)
	// The fake client writes no outfile, so the job runs again on the next
	// scheduler run after a success.
	b.statuses = []int{1, 1, 1, 0, 1, 1, 1}

	b.runEveryMinute(12)

	want := []int{0, 1, 3, 7, 8, 9, 11}
	if got := b.minutesOfRuns(start); !slices.Equal(got, want) {
		t.Fatalf("job ran at minutes %v, want %v", got, want)
	}
}

// TestSchedulerBackoffEndsWhenTheOutfileChanges checks that a failing job
// whose outfile has the date in it runs at once when the date changes, as
// that is another result; the TimeRange [0, 24) of the failed runs of the
// day before ends then too, so a final run for the outfile of the day before
// comes first, and then backs off on its own.
func TestSchedulerBackoffEndsWhenTheOutfileChanges(t *testing.T) {
	start := time.Date(2026, 9, 22, 23, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result-$today.csv"), start)
	b.statuses = slices.Repeat([]int{1}, 15)

	b.runEveryMinute(125)

	// 0, 1, 3, 7, 15, 31 fail on the 22nd; at midnight (minute 60) its
	// final run fails too (its 7th failure: an hour of backoff, so its next
	// final run is at 120), and the outfile is the one of the 23rd, whose
	// runs back off from 1 minute again.
	want := []int{0, 1, 3, 7, 15, 31, 60, 60, 61, 63, 67, 75, 91, 120, 123}
	if got := b.minutesOfRuns(start); !slices.Equal(got, want) {
		t.Fatalf("job ran at minutes %v, want %v", got, want)
	}
	s, p := clients.ScheduledMode, clients.ScheduledPartialMode
	wantModes := []clients.MaprClientMode{s, s, s, s, s, s, p, s, s, s, s, s, s, p, s}
	if !slices.Equal(b.modes, wantModes) {
		t.Fatalf("job ran with modes %v, want %v", b.modes, wantModes)
	}
}

// TestSchedulerBacksOffALongFailingJob checks that a failing job that runs
// longer than its backoff does not run back to back: after its run it waits
// at least half its backoff.
func TestSchedulerBacksOffALongFailingJob(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result.csv"), start)
	b.runTime = 10*time.Minute + 20*time.Second
	b.statuses = slices.Repeat([]int{1}, 7)

	b.runEveryMinute(120)

	// Run 1 ends at 10:20, backoff 1m: from 10:50 (half after its end), so
	// at 11. Run 2 ends at 21:20, 2m: from 22:20, so at 23. Run 3 ends at
	// 33:20, 4m: from 35:20, so at 36. Run 4 (8m) starts at 36, ends at
	// 46:20: from 50:20 (half after the end), so at 51. Run 5 (16m) ends at
	// 61:20: from 69:20, so at 70. Run 6 (32m) ends at 80:20: from 102
	// (the backoff after its start).
	want := []int{0, 11, 23, 36, 51, 70, 102}
	if got := b.minutesOfRuns(start); !slices.Equal(got, want) {
		t.Fatalf("job ran at minutes %v, want %v", got, want)
	}
}

// TestSchedulerBackoffAllowsAFewSecondsOfSlack checks that a job whose run
// failed runs on the scheduler run one backoff later, also when that run
// comes a few seconds before the backoff ends, but not earlier.
func TestSchedulerBackoffAllowsAFewSecondsOfSlack(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result.csv"), start)
	b.statuses = []int{1, 1}

	b.runJobs(context.Background())
	for _, offset := range []time.Duration{30 * time.Second, 54 * time.Second, 55 * time.Second} {
		b.now = start.Add(offset)
		b.runJobs(context.Background())
	}

	want := []time.Time{start, start.Add(55 * time.Second)}
	if !slices.Equal(b.runs, want) {
		t.Fatalf("job ran at %v, want %v", b.runs, want)
	}
}

func countLines(lines []string, prefix string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// TestBackoffStartsOverInANewTimeRange covers an outfile without dates whose
// final runs failed on one day and whose job fails again in the next day's
// TimeRange: the new range's failure starts over, so that its first final run
// comes at once after the range ends instead of after the previous range's
// hour-long backoff.
func TestBackoffStartsOverInANewTimeRange(t *testing.T) {
	job := &config.Scheduled{TimeRange: [2]int{1, 2}}
	job.Name = "dateless"
	due := dueJob{job: job, outfile: "latest.csv"}
	day1 := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	at := func(day time.Time, hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	var backoff jobBackoff

	backoff.fail(due, false, at(day1, 1, 30), at(day1, 1, 30), at(day1, 2, 0))
	// Day 1's final runs keep failing until the backoff is an hour long.
	for minute := 0; minute < 8; minute++ {
		backoff.fail(due, true, at(day1, 2, minute), at(day1, 2, minute), at(day1, 2, 0))
	}
	// The hourly final runs go on, and one fails shortly before day 2's
	// range: its backoff lasts until 01:30, past the start of day 2's range.
	backoff.fail(due, true, at(day2, 0, 30), at(day2, 0, 30), at(day1, 2, 0))
	// Day 1's hour-long backoff must not hold back day 2's first run.
	if reason := backoff.wait(job, due.outfile, at(day2, 1, 0)); reason != "" {
		t.Fatalf("day 2's first run was held back by day 1's backoff: %s", reason)
	}
	// Within one range the backoff still applies.
	if reason := backoff.wait(job, due.outfile, at(day1, 1, 45)); reason == "" {
		t.Fatal("a retry within the failed run's TimeRange was not held back")
	}
	state := backoff.fail(due, false, at(day2, 1, 10), at(day2, 1, 10), at(day2, 2, 0))
	if state.failures != 1 || state.finalRuns != 0 {
		t.Fatalf("after the new range's failure: failures = %d, finalRuns = %d, want 1 and 0",
			state.failures, state.finalRuns)
	}
	if want := at(day2, 1, 11); !state.retryAt.Equal(want) {
		t.Errorf("retryAt = %s, want %s (the first failure's backoff)", state.retryAt, want)
	}
	due2, _ := backoff.finalRunsDue(at(day2, 2, 0), func(failedJob) bool { return false })
	if len(due2) != 1 {
		t.Fatalf("final runs due when day 2's range ends = %d, want 1", len(due2))
	}
}
