package jobs

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

type scheduler struct {
	cfg           config.RuntimeConfig
	logger        logging.Logger
	newMaprClient func(config.Args, clients.MaprClientMode) (backgroundClient, error)
	// now is the clock the time ranges and the dates in file names and
	// outfiles are evaluated with.
	now func() time.Time
	// thisDServer recognises the servers of jobs that reach the dserver
	// running the scheduler.
	thisDServer thisDServer
	// backoff delays the next runs of jobs whose runs failed.
	backoff jobBackoff
}

func newScheduler(cfg config.RuntimeConfig, loggers clients.LoggerDependencies, colorizers ...*brush.Brush) *scheduler {
	colorizer := firstColorizer(colorizers)
	return &scheduler{
		cfg:         cfg,
		logger:      logging.OrNop(loggers.Server),
		now:         time.Now,
		thisDServer: newThisDServer(cfg),
		newMaprClient: func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
			return clients.NewMaprClient(args, cfg, mode, loggers, colorizer)
		},
	}
}

func (s *scheduler) log() logging.Logger {
	if s.logger == nil {
		return logging.NopLogger{}
	}
	return s.logger
}

func (s *scheduler) start(ctx context.Context) {
	s.log().Info("Starting scheduled job runner after 2s")
	if !wait(ctx, 2*time.Second) {
		return
	}
	s.runJobs(ctx)
	runTicker := time.NewTicker(time.Minute)
	defer runTicker.Stop()
	for {
		select {
		case <-runTicker.C:
			s.runJobs(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// runJobs runs the final runs that are due (see runFinalJobs) and then the
// enabled jobs that are due, one group after another (see nextGroup). Like
// the scheduler that ran every job on its own, it checks a job's time range,
// fills the dates into its files and outfile and checks that the outfile does
// not exist yet right before the job's group starts.
func (s *scheduler) runJobs(ctx context.Context) {
	limits := newGroupLimits()
	s.runFinalJobs(ctx, limits)
	var pending []pendingRun
	for i := range s.cfg.Server.Schedule {
		job := &s.cfg.Server.Schedule[i]
		if !job.Enable {
			s.log().Debug(job.Name, "Not running job as not enabled")
			continue
		}
		pending = append(pending, scheduledRun{job: job})
	}
	s.runPending(ctx, pending, limits)
}

// evaluate returns the client arguments of job at now, or why job is not due:
// now is outside its time range, its outfile already exists, or its last run
// failed and its backoff (see jobBackoff) has not ended yet.
func (s *scheduler) evaluate(job *config.Scheduled, now time.Time) (dueJob, string) {
	if hour := now.Hour(); hour < job.TimeRange[0] || hour >= job.TimeRange[1] {
		return dueJob{}, "Not running job out of time range"
	}
	due, reason := s.prepare(job, now)
	if reason != "" {
		return dueJob{}, reason
	}
	if reason := s.backoff.wait(job, due.outfile, now); reason != "" {
		return dueJob{}, reason
	}
	return due, ""
}

// prepare returns the client arguments of job at now, or why job is not due:
// its outfile already exists.
func (s *scheduler) prepare(job *config.Scheduled, now time.Time) (dueJob, string) {
	files := fillDatesAt(job.Files, now)
	outfile := fillDatesAt(job.Outfile, now)

	_, err := os.Stat(outfile)
	if !os.IsNotExist(err) {
		return dueJob{}, "Not running job as outfile already exists: " + outfile
	}

	servers := strings.Join(job.Servers, ",")
	if servers == "" {
		servers = s.cfg.Server.SSHBindAddress
	}
	args := config.Args{
		ConnectionsPerCPU: config.DefaultConnectionsPerCPU,
		Discovery:         job.Discovery,
		ServersStr:        servers,
		What:              files,
		Mode:              omode.MapClient,
		SSHArgs: config.SSHArgs{
			NoAuthKey: true,
			UserName:  config.ScheduleUser,
		},
	}

	args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(job.Name))
	args.QueryStr = fmt.Sprintf("%s outfile %s", job.Query, outfile)
	return dueJob{job: job, args: args, outfile: outfile, rangeEnd: timeRangeEnd(job, now)}, ""
}

// runFinalJobs runs the final runs of the jobs whose runs failed and whose
// TimeRange ended since (see jobBackoff): they write what they could read,
// unless their outfile exists by now. They run in the order of their jobs in
// the schedule and form groups by the same rules, and bounded by the same
// waves, as the runs within the jobs' TimeRange (see nextGroup), with the
// files and the outfiles of their failed runs; each group shares its reads.
func (s *scheduler) runFinalJobs(ctx context.Context, limits *groupLimits) {
	now := s.now()
	due, expired := s.backoff.finalRunsDue(now, func(state failedJob) bool {
		job := state.due.job
		hour := now.Hour()
		return hour >= job.TimeRange[0] && hour < job.TimeRange[1] && fillDatesAt(job.Outfile, now) == state.due.outfile
	})
	for _, state := range expired {
		s.log().Warn(fmt.Sprintf("Giving up job %s after %d failures in a row: it wrote no outfile %s "+
			"within %v after its TimeRange ended at %s", state.due.job.Name, state.failures, state.due.outfile,
			failedJobFinalRunWindow, state.rangeEnd.Format(time.DateTime)))
	}
	// finalRunsDue sorts by job name and outfile; order the runs of the
	// jobs as in the schedule, which the conflict rules of nextGroup keep.
	slices.SortStableFunc(due, func(a, b failedJob) int {
		return cmp.Compare(s.scheduleIndex(a.due.job), s.scheduleIndex(b.due.job))
	})
	pending := make([]pendingRun, len(due))
	for i, state := range due {
		pending[i] = finalRun{state: state}
	}
	s.runPending(ctx, pending, limits)
}

// scheduleIndex returns the index of job in the schedule, or -1.
func (s *scheduler) scheduleIndex(job *config.Scheduled) int {
	for i := range s.cfg.Server.Schedule {
		if &s.cfg.Server.Schedule[i] == job {
			return i
		}
	}
	return -1
}

func (s *scheduler) runDueJob(ctx context.Context, due dueJob) {
	job := due.job
	mode := clients.ScheduledMode
	if due.final {
		mode = clients.ScheduledPartialMode
	}
	client, err := s.newMaprClient(due.args, mode)
	if err != nil {
		s.log().Error(fmt.Sprintf("Unable to create job %s", job.Name), err)
		now := s.now()
		s.logFailure(due, s.backoff.fail(due, due.final, now, now, due.rangeEnd))
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if due.final {
		s.log().Info(fmt.Sprintf("Starting final run of job %s for outfile %s after its TimeRange ended at %s, "+
			"it writes what it can read unless a server fails", job.Name, due.outfile,
			due.rangeEnd.Format(time.DateTime)))
	}
	s.log().Info(fmt.Sprintf("Starting job %s", job.Name))
	started := s.now()
	status := client.Start(jobCtx, make(chan string))
	logMessage := fmt.Sprintf("Job %s exited with status %d", job.Name, status)

	if status != 0 {
		// A scheduled mapreduce client writes the outfile only when it
		// returns status 0, and the outfile did not exist when the job
		// started: a later run finds none and runs the job again, once the
		// job's backoff ended, or once its TimeRange ended (a final run).
		s.log().Warn(logMessage)
		s.logFailure(due, s.backoff.fail(due, due.final, started, s.now(), due.rangeEnd))
		return
	}

	s.backoff.succeed(job, due.outfile)
	s.log().Info(logMessage)
}

// logFailure logs that the run of due failed, which makes state its failures
// in a row. The backoff bounds how often a job fails and so logs this.
func (s *scheduler) logFailure(due dueJob, state failedJob) {
	next := state.retryAt
	if !due.final && state.rangeEnd.Before(next) {
		next = state.rangeEnd
	}
	s.log().Warn(fmt.Sprintf("Job %s failed and wrote no outfile %s (failure %d in a row), it runs again "+
		"from about %s", due.job.Name, due.outfile, state.failures, next.Format(time.DateTime)))
}
