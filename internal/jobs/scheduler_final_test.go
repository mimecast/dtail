package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

const finalTestQuery = "from STATS select count($line),max(lifetimeConnections) group by $hostname"

// clientWarnings records the warnings of the mapreduce clients.
type clientWarnings struct {
	clientlog.NopLogger
	mu    sync.Mutex
	lines []string
}

func (l *clientWarnings) Warn(args ...any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprint(args...))
	return ""
}

func (l *clientWarnings) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return countLines(l.lines, prefix)
}

// finalTestScheduler is a scheduler with a fake clock and a single job run
// with real mapreduce clients.
type finalTestScheduler struct {
	*scheduler
	now      time.Time
	logger   *exitLogger
	warnings *clientWarnings
	modes    []clients.MaprClientMode
	// outfiles are the outfiles of the runs.
	outfiles []string
}

// newFinalTestScheduler returns a scheduler for job whose clients run
// serverless (reading the files in-process) unless the job has servers, which
// they then connect to.
func newFinalTestScheduler(t *testing.T, job config.Scheduled, start time.Time) *finalTestScheduler {
	t.Helper()
	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
	cfg.Server.ReadGlobRetryIntervalMs = 10
	job.Enable = true
	job.Query = finalTestQuery
	cfg.Server.Schedule = []config.Scheduled{job}

	f := &finalTestScheduler{now: start, logger: &exitLogger{}, warnings: &clientWarnings{}}
	loggers := clients.NewLoggerDependencies(f.warnings, logging.NopLogger{}, logging.NopLogger{})
	f.scheduler = newScheduler(cfg, loggers)
	f.scheduler.logger = f.logger
	f.scheduler.now = func() time.Time { return f.now }
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	f.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		f.modes = append(f.modes, mode)
		f.outfiles = append(f.outfiles, args.QueryStr[strings.LastIndex(args.QueryStr, " outfile ")+len(" outfile "):])
		args.KnownHostsPath = knownHosts
		args.Serverless = len(job.Servers) == 0
		return clients.NewMaprClient(args, cfg, mode, loggers)
	}
	return f
}

// runAt runs the scheduler at the given time of the fake clock's day.
func (f *finalTestScheduler) runAt(hour, minute int) {
	f.now = time.Date(f.now.Year(), f.now.Month(), f.now.Day(), hour, minute, 0, 0, f.now.Location())
	f.runJobs(context.Background())
}

func writeStatsLog(t *testing.T, path string, lines int) {
	t.Helper()
	content := strings.Repeat("INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
		"currentConnections=0|lifetimeConnections=1\n", lines)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertNoJobFiles(t *testing.T, outfile, when string) {
	t.Helper()
	for _, path := range []string{outfile, outfile + ".query", outfile + ".tmp", outfile + ".query.tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %s exists (stat error %v), want none", when, path, err)
		}
	}
}

// TestSchedulerWritesPartialResultAfterTimeRangeEnds runs a job reading a
// list of two files, one of which never appears, with a TimeRange of 1 to 2
// o'clock. Within the TimeRange its runs write no outfile. At the first
// scheduler run from 2 o'clock on, a final run writes the result of the file
// that exists, once, and logs that it is partial.
func TestSchedulerWritesPartialResultAfterTimeRangeEnds(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.log")
	writeStatsLog(t, present, 3)
	outfile := filepath.Join(dir, "result.csv")
	job := config.Scheduled{}
	job.Name = "list"
	job.TimeRange = [2]int{1, 2}
	job.Files = present + "," + filepath.Join(dir, "missing.log")
	job.Outfile = outfile
	f := newFinalTestScheduler(t, job, time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local))

	f.runAt(0, 59) // Before the TimeRange: no run.
	for _, minute := range []int{0, 1, 3, 7, 15, 31} {
		f.runAt(1, minute)
		assertNoJobFiles(t, outfile, fmt.Sprintf("run at 1:%02d within the TimeRange", minute))
	}
	f.runAt(1, 59) // Within the backoff: no run.
	if got := countLines(f.logger.lines, "Job list failed and wrote no outfile"); got != 6 {
		t.Fatalf("logged %d failures within the TimeRange, want 6: %q", got, f.logger.lines)
	}
	if got := f.warnings.count("Not writing the mapreduce result as files could not be read"); got != 6 {
		t.Fatalf("clients logged %d blocked results, want 6: %q", got, f.warnings.lines)
	}

	f.runAt(2, 0)
	assertJobFile(t, outfile, "count($line),max(lifetimeConnections)\n3,1.000000\n")
	assertJobFile(t, outfile+".query", finalTestQuery+" outfile "+outfile)
	if got := f.warnings.count("Writing partial mapreduce result after the job's TimeRange ended"); got != 1 {
		t.Fatalf("clients logged %d partial results, want 1: %q", got, f.warnings.lines)
	}
	wantStart := "Starting final run of job list for outfile " + outfile + " after its TimeRange ended at " +
		"2026-09-22 02:00:00, it writes what it can read unless a server fails"
	if !slices.Contains(f.logger.lines, wantStart) {
		t.Fatalf("log = %q, want %q", f.logger.lines, wantStart)
	}

	for _, at := range [][2]int{{2, 1}, {3, 0}, {23, 59}} {
		f.runAt(at[0], at[1])
	}
	s, p := clients.ScheduledMode, clients.ScheduledPartialMode
	if want := []clients.MaprClientMode{s, s, s, s, s, s, p}; !slices.Equal(f.modes, want) {
		t.Fatalf("job ran with modes %v, want %v", f.modes, want)
	}
}

// TestSchedulerWritesCompleteResultWhenTheFileAppearsBeforeTheFinalRun
// checks that a final run whose files all exist by then writes the complete
// result and logs no partial result.
func TestSchedulerWritesCompleteResultWhenTheFileAppearsBeforeTheFinalRun(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.log"), filepath.Join(dir, "second.log")
	writeStatsLog(t, first, 3)
	outfile := filepath.Join(dir, "result.csv")
	job := config.Scheduled{}
	job.Name = "late"
	job.TimeRange = [2]int{1, 2}
	job.Files = first + "," + second
	job.Outfile = outfile
	f := newFinalTestScheduler(t, job, time.Date(2026, 9, 22, 0, 0, 0, 0, time.Local))

	f.runAt(1, 30)
	assertNoJobFiles(t, outfile, "run within the TimeRange")
	writeStatsLog(t, second, 2)
	f.runAt(2, 0)

	assertJobFile(t, outfile, "count($line),max(lifetimeConnections)\n5,1.000000\n")
	if got := f.warnings.count("Writing partial"); got != 0 {
		t.Fatalf("clients logged %d partial results, want none: %q", got, f.warnings.lines)
	}
}

// TestSchedulerNeverWritesPartialResultAfterTransportFailure runs a job
// whose server can not be reached, with a TimeRange of 1 to 2 o'clock and an
// outfile of the day. The final runs for the outfile of the 22nd after the
// TimeRange ended still write nothing; they back off and the scheduler gives
// up on the outfile a day after the TimeRange ended. The runs for the outfile
// of the 23rd (within its TimeRange on the 23rd) fail on their own.
func TestSchedulerNeverWritesPartialResultAfterTransportFailure(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")
	writeStatsLog(t, logFile, 3)
	job := config.Scheduled{}
	job.Name = "unreachable"
	job.TimeRange = [2]int{1, 2}
	job.Servers = []string{closedTCPAddress(t)}
	job.Files = logFile
	job.Outfile = filepath.Join(dir, "result-$today.csv")
	start := time.Date(2026, 9, 22, 1, 58, 0, 0, time.Local)
	f := newFinalTestScheduler(t, job, start)
	outfile := filepath.Join(dir, "result-20260922.csv")

	var runs []string
	for tick := start; tick.Before(start.Add(26 * time.Hour)); tick = tick.Add(time.Minute) {
		f.now = tick
		before := len(f.outfiles)
		f.runJobs(context.Background())
		for i := before; i < len(f.outfiles); i++ {
			if f.outfiles[i] == outfile {
				runs = append(runs, fmt.Sprintf("%s %v", tick.Format("02 15:04"), f.modes[i]))
			}
		}
		if files, _ := filepath.Glob(filepath.Join(dir, "result-*")); len(files) > 0 {
			t.Fatalf("run at %s left %v", tick.Format(time.DateTime), files)
		}
	}

	// 1:58 and 1:59 within the TimeRange; final runs from 2:00 on, backing
	// off from the job's 3rd failure (4 minutes) up to an hour, until the
	// scheduler gives up at 2:00 on the 23rd.
	s, p := clients.ScheduledMode, clients.ScheduledPartialMode
	want := []string{fmt.Sprintf("22 01:58 %v", s), fmt.Sprintf("22 01:59 %v", s)}
	for _, at := range []string{"02:00", "02:04", "02:12", "02:28", "03:00"} {
		want = append(want, fmt.Sprintf("22 %s %v", at, p))
	}
	for hour := 4; hour <= 23; hour++ {
		want = append(want, fmt.Sprintf("22 %02d:00 %v", hour, p))
	}
	for hour := 0; hour <= 1; hour++ {
		want = append(want, fmt.Sprintf("23 %02d:00 %v", hour, p))
	}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs for %s at\n%v\nwant\n%v", outfile, runs, want)
	}
	if got := f.warnings.count("Writing partial"); got != 0 {
		t.Fatalf("clients logged %d partial results, want none: %q", got, f.warnings.lines)
	}
	wantGiveUp := "Giving up job unreachable after 29 failures in a row: it wrote no outfile " + outfile +
		" within 24h0m0s after its TimeRange ended at 2026-09-22 02:00:00"
	if got := countLines(f.logger.lines, wantGiveUp); got != 1 {
		t.Fatalf("logged %q %d times, want once: %q", wantGiveUp, got, f.logger.lines)
	}
}

// TestSchedulerReadsAGlobWithASubdirectory runs a job whose glob matches a
// file and a directory. The directory is no file to read, not a failure: the
// first run within the TimeRange writes the complete result.
func TestSchedulerReadsAGlobWithASubdirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs", "archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeStatsLog(t, filepath.Join(dir, "logs", "app.log"), 4)
	outfile := filepath.Join(dir, "result.csv")
	job := config.Scheduled{}
	job.Name = "dirglob"
	job.TimeRange = [2]int{0, 24}
	job.Files = filepath.Join(dir, "logs", "*")
	job.Outfile = outfile
	f := newFinalTestScheduler(t, job, time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local))

	f.runAt(1, 0)

	assertJobFile(t, outfile, "count($line),max(lifetimeConnections)\n4,1.000000\n")
	if len(f.warnings.lines) != 0 {
		t.Fatalf("clients logged warnings %q, want none", f.warnings.lines)
	}
}

// TestSchedulerRunsAnUndatedOutfileStrictlyInItsNextTimeRange checks a
// failing job whose outfile has no date: after its TimeRange ended its final
// runs may write a partial result, but once its TimeRange of the next day
// started, its runs read every file again until that TimeRange ended.
func TestSchedulerRunsAnUndatedOutfileStrictlyInItsNextTimeRange(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result.csv"), start)
	b.cfg.Server.Schedule[0].TimeRange = [2]int{1, 2}
	b.statuses = slices.Repeat([]int{1}, 100)

	b.runEveryMinute(26 * 60)

	sawFinalOnDay := map[int]bool{}
	for i, run := range b.runs {
		inRange := run.Hour() == 1
		if final := b.modes[i] == clients.ScheduledPartialMode; final == inRange {
			t.Fatalf("run at %v has mode %v, want the partial mode exactly outside of its TimeRange",
				run, b.modes[i])
		}
		if run.Hour() == 2 && run.Minute() == 0 {
			sawFinalOnDay[run.Day()] = true
		}
	}
	if !sawFinalOnDay[22] || !sawFinalOnDay[23] {
		t.Fatalf("final runs at 2:00 on days %v, want the 22nd and the 23rd (runs %v)", sawFinalOnDay, b.runs)
	}
}

func TestTimeRangeEnd(t *testing.T) {
	t.Parallel()

	job := &config.Scheduled{}
	now := time.Date(2026, 9, 22, 13, 37, 0, 0, time.Local)
	for _, tt := range []struct {
		end  int
		want time.Time
	}{
		{end: 14, want: time.Date(2026, 9, 22, 14, 0, 0, 0, time.Local)},
		{end: 24, want: time.Date(2026, 9, 23, 0, 0, 0, 0, time.Local)},
	} {
		job.TimeRange = [2]int{0, tt.end}
		if got := timeRangeEnd(job, now); !got.Equal(tt.want) {
			t.Errorf("timeRangeEnd(%v) = %v, want %v", job.TimeRange, got, tt.want)
		}
	}
}

// TestSchedulerSkipsTheFinalRunOnceTheOutfileExists checks that a failed job
// whose outfile was written by something else before its TimeRange ended
// has no final run, and no runs later on.
func TestSchedulerSkipsTheFinalRunOnceTheOutfileExists(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	outfile := filepath.Join(t.TempDir(), "result.csv")
	b := newBackoffTestScheduler(t, outfile, start)
	b.cfg.Server.Schedule[0].TimeRange = [2]int{1, 2}
	b.statuses = []int{1}

	b.runJobs(context.Background())
	if err := os.WriteFile(outfile, []byte("written elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.runEveryMinute(3 * 60)

	if want := []time.Time{start}; !slices.Equal(b.runs, want) {
		t.Fatalf("job ran at %v, want %v", b.runs, want)
	}
	if len(b.backoff.failed) != 0 {
		t.Fatalf("backoff still holds %v", b.backoff.failed)
	}
}

// TestFinalRunsDueOnlyAfterTheTimeRangeEnded checks that a failed job has no
// final run before the end of its TimeRange, also when the scheduler does not
// run it within its TimeRange any more.
func TestFinalRunsDueOnlyAfterTheTimeRangeEnded(t *testing.T) {
	t.Parallel()

	job := &config.Scheduled{}
	job.Name = "job"
	end := time.Date(2026, 9, 22, 2, 0, 0, 0, time.Local)
	var b jobBackoff
	b.fail(dueJob{job: job, outfile: "out.csv"}, false, end.Add(-time.Hour), end.Add(-time.Hour), end)
	notInRange := func(failedJob) bool { return false }

	for _, tt := range []struct {
		at   time.Time
		want int
	}{
		{at: end.Add(-time.Second)},
		{at: end, want: 1},
	} {
		if due, expired := b.finalRunsDue(tt.at, notInRange); len(due) != tt.want || len(expired) != 0 {
			t.Errorf("finalRunsDue(%v) = %v, %v, want %d due", tt.at, due, expired, tt.want)
		}
	}
}

// TestSchedulerFinalRunReadsTheFilesOfTheLastFailedRun runs a job with an
// outfile without dates and files of the day that fails on the 22nd and on
// the 23rd: the final run after the TimeRange of the 23rd reads the files of
// the 23rd, not those of the 22nd, the day of its first failure.
func TestSchedulerFinalRunReadsTheFilesOfTheLastFailedRun(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	b := newBackoffTestScheduler(t, filepath.Join(t.TempDir(), "result.csv"), start)
	b.cfg.Server.Schedule[0].TimeRange = [2]int{1, 2}
	b.cfg.Server.Schedule[0].Files = "/logs/app-$today.log"
	b.statuses = slices.Repeat([]int{1}, 100)
	newClient := b.newMaprClient
	type run struct {
		at    time.Time
		files string
	}
	var finals []run
	b.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		if mode == clients.ScheduledPartialMode {
			finals = append(finals, run{at: b.now, files: args.What})
		}
		return newClient(args, mode)
	}

	b.runEveryMinute(25*60 + 1)

	// The final runs until the TimeRange of the 23rd starts are those of the
	// failed runs of the 22nd; the one when it ended is of the 23rd.
	secondRange := time.Date(2026, 9, 23, 1, 0, 0, 0, time.Local)
	if len(finals) < 2 || !finals[len(finals)-1].at.Equal(secondRange.Add(time.Hour)) {
		t.Fatalf("final runs %v, want some on the 22nd and the last at %v", finals, secondRange.Add(time.Hour))
	}
	for _, final := range finals {
		want := "/logs/app-20260922.log"
		if final.at.After(secondRange) {
			want = "/logs/app-20260923.log"
		}
		if final.files != want {
			t.Errorf("final run at %v read %s, want %s", final.at, final.files, want)
		}
	}
}
