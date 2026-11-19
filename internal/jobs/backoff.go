package jobs

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

const (
	// failedJobInitialBackoff is how long the scheduler waits after a job's
	// first failed run before it runs the job again. Every further failure in
	// a row doubles the wait, up to failedJobMaxBackoff.
	failedJobInitialBackoff = time.Minute
	failedJobMaxBackoff     = time.Hour
	// failedJobBackoffSlack lets a job run on a scheduler run that comes a
	// little before its backoff ends. The scheduler runs every minute, but
	// its runs, and the start of a job within them, drift by a little: the
	// slack keeps a job from missing the scheduler run one backoff after
	// its failed run by that little and waiting a whole minute longer.
	failedJobBackoffSlack = 5 * time.Second
	// failedJobFinalRunWindow is how long after the end of its TimeRange the
	// scheduler keeps running a failed job for its final result (see
	// jobBackoff), before it gives up on that outfile.
	failedJobFinalRunWindow = 24 * time.Hour
)

// jobBackoff holds, in memory, the scheduled jobs whose last run for an
// outfile failed, when the scheduler may run each again, and until when a
// run must read every file (the end of the job's TimeRange).
//
// A failed job wrote no outfile, so the scheduler would otherwise run it
// again every minute, and a job that fails for good (e.g. one of its servers
// is gone) would read the files of all its other servers every minute. The
// failures of a job end when it writes its outfile.
//
// Within its TimeRange a job writes its outfile only once it read every file
// (clients.ScheduledMode). Once the TimeRange in which a job's run for an
// outfile failed ended, the scheduler runs the job for that outfile again, at
// its first run from then on and then after its backoff, with the files and
// the outfile of the failed run; those final runs write what they could read
// if files were missing or unreadable (clients.ScheduledPartialMode), but
// still nothing when a server could not be reached or its session was cut
// short. It gives up on the outfile failedJobFinalRunWindow after the end of
// the TimeRange.
type jobBackoff struct {
	mu     sync.Mutex
	failed map[failedJobKey]failedJob
}

// failedJobKey is a job's result: its outfile, with its dates filled in.
type failedJobKey struct {
	job     *config.Scheduled
	outfile string
}

// failedJob is a job whose runs for an outfile failed failures times in a row.
type failedJob struct {
	// due is the job's last run within its TimeRange that failed, without a
	// read share: its files and outfile have the dates of that run filled in,
	// and its final runs read those files.
	due      dueJob
	failures int
	// retryAt is when the job may run again: the backoff after the start of
	// its last failed run, but at least half the backoff after its end, so
	// that a job running longer than its backoff does not run back to back.
	retryAt time.Time
	// rangeEnd is when the TimeRange of the job's last failed run within its
	// TimeRange ended; its runs from then on are final runs.
	rangeEnd time.Time
	// finalRuns is how many final runs failed.
	finalRuns int
}

func (f failedJob) key() failedJobKey {
	return failedJobKey{job: f.due.job, outfile: f.due.outfile}
}

// backoffDelay returns how long the scheduler waits before it runs a job
// again that failed failures times in a row: 1, 2, 4, ... minutes, at most an
// hour.
func backoffDelay(failures int) time.Duration {
	delay := failedJobInitialBackoff
	for i := 1; i < failures && delay < failedJobMaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, failedJobMaxBackoff)
}

// timeRangeEnd returns when the TimeRange of job that now is in ends.
func timeRangeEnd(job *config.Scheduled, now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), job.TimeRange[1], 0, 0, 0, now.Location())
}

// wait returns why job, due within its TimeRange at now with outfile, must
// not run yet, or "" if it may run.
func (b *jobBackoff) wait(job *config.Scheduled, outfile string, now time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.failed[failedJobKey{job: job, outfile: outfile}]
	if !ok {
		return ""
	}
	if !timeRangeEnd(job, now).Equal(state.rangeEnd) {
		// A run in a new TimeRange (the next day's, for an outfile without
		// dates) starts over: the previous range's backoff must not hold it
		// back, or a short range could pass without any run.
		return ""
	}
	if now.Add(failedJobBackoffSlack).Before(state.retryAt) {
		return fmt.Sprintf("Not running job after failure %d in a row before about %s",
			state.failures, state.retryAt.Format(time.DateTime))
	}
	return ""
}

// fail records that the run due, which started at started, failed and ended
// at ended. final tells whether it was a final run, otherwise it ran within
// the job's TimeRange, which ends at rangeEnd. It returns the job's failures
// for due's outfile now.
func (b *jobBackoff) fail(due dueJob, final bool, started, ended, rangeEnd time.Time) failedJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed == nil {
		b.failed = make(map[failedJobKey]failedJob)
	}
	key := failedJobKey{job: due.job, outfile: due.outfile}
	state, ok := b.failed[key]
	if ok && !final && !state.rangeEnd.Equal(rangeEnd) {
		// The failure opens a new TimeRange (e.g. the next day's, for an
		// outfile without dates): it starts over, so that its backoff does
		// not carry the previous range's, and its first final run comes at
		// once after the new range ends.
		state.failures = 0
		state.finalRuns = 0
	}
	if !ok || !final {
		// A later run within the TimeRange, e.g. the next day's for an
		// outfile without dates, may read other files: the final runs read
		// the files of the last one. They share reads only within the
		// group they run in.
		state.due = due
		state.due.args.ReadShare = config.ReadShare{}
	}
	state.failures++
	if final {
		state.finalRuns++
	} else {
		state.rangeEnd = rangeEnd
	}
	delay := backoffDelay(state.failures)
	state.retryAt = started.Add(delay)
	if afterEnd := ended.Add(delay / 2); afterEnd.After(state.retryAt) {
		state.retryAt = afterEnd
	}
	b.failed[key] = state
	return state
}

// succeed forgets the failures of job for outfile.
func (b *jobBackoff) succeed(job *config.Scheduled, outfile string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failed, failedJobKey{job: job, outfile: outfile})
}

// forget forgets state.
func (b *jobBackoff) forget(state failedJob) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failed, state.key())
}

// finalRunsDue returns the failed jobs whose TimeRange ended by now and that
// may run their final run at now: the first one at once, the others after
// their backoff. It leaves out the jobs inRange reports as running within
// their TimeRange again at now for the same outfile (possible for an outfile
// without dates): those runs must read every file again. It forgets, and
// returns as expired, the jobs whose final runs failed for
// failedJobFinalRunWindow after the end of their TimeRange. Both are sorted
// by job name and outfile.
func (b *jobBackoff) finalRunsDue(now time.Time, inRange func(failedJob) bool) (due, expired []failedJob) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, state := range b.failed {
		switch {
		case now.Before(state.rangeEnd) || inRange(state):
		case !now.Before(state.rangeEnd.Add(failedJobFinalRunWindow)):
			delete(b.failed, key)
			expired = append(expired, state)
		case state.finalRuns == 0 || !now.Add(failedJobBackoffSlack).Before(state.retryAt):
			due = append(due, state)
		}
	}
	byJob := func(a, b failedJob) int {
		return cmp.Or(cmp.Compare(a.due.job.Name, b.due.job.Name), cmp.Compare(a.due.outfile, b.due.outfile))
	}
	slices.SortFunc(due, byJob)
	slices.SortFunc(expired, byJob)
	return due, expired
}
