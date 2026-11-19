package jobs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	gossh "golang.org/x/crypto/ssh"
)

type backgroundClient interface {
	Start(context.Context, <-chan string) int
}

type continuous struct {
	cfg              config.RuntimeConfig
	logger           logging.Logger
	newMaprClient    func(config.Args, clients.MaprClientMode) (backgroundClient, error)
	dayChangeWatcher func(context.Context, time.Time) bool
	retryInterval    time.Duration
	now              func() time.Time
	newTicker        func(time.Duration) (<-chan time.Time, func())
}

func newContinuous(cfg config.RuntimeConfig, loggers clients.LoggerDependencies, colorizers ...*brush.Brush) *continuous {
	colorizer := firstColorizer(colorizers)
	c := &continuous{cfg: cfg, logger: logging.OrNop(loggers.Server)}
	c.retryInterval = time.Minute
	c.now = time.Now
	c.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		ticker := time.NewTicker(d)
		return ticker.C, ticker.Stop
	}
	c.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		return clients.NewMaprClient(args, cfg, mode, loggers, colorizer)
	}
	c.dayChangeWatcher = c.waitForDayChange
	return c
}

func (c *continuous) log() logging.Logger {
	if c.logger == nil {
		return logging.NopLogger{}
	}
	return c.logger
}

func (c *continuous) start(ctx context.Context) {
	c.log().Info("Starting continuous job runner after 2s")
	if !wait(ctx, 2*time.Second) {
		return
	}
	c.runJobs(ctx)
}

func (c *continuous) runJobs(ctx context.Context) {
	var workers sync.WaitGroup
	for i := range c.cfg.Server.Continuous {
		job := &c.cfg.Server.Continuous[i]
		if !job.Enable {
			c.log().Debug(job.Name, "Not running job as not enabled")
			continue
		}
		workers.Add(1)
		go func(job *config.Continuous) {
			defer workers.Done()
			// A job restarted for a day change runs again at once: tail
			// mode starts at the end of the new day's files, so every
			// second waited is missing from the result. (The first such
			// restart used to wait a whole retry interval, as the retry
			// ticker only started after the first run.) Any other exit
			// retries after the configured interval, unless the server
			// shuts down.
			for {
				if c.runJob(ctx, job) {
					continue
				}
				if !ctxutil.Sleep(ctx, c.retryInterval) {
					return
				}
			}
		}(job)
	}
	// runJobs owns the retry workers it starts. Joining them makes context
	// cancellation a real lifecycle boundary for callers and prevents workers
	// from outliving server/test resources such as the process logger.
	workers.Wait()
}

// runJob runs job once and reports whether it ended because the day changed
// (RestartOnDayChange), in which case the caller starts it again at once.
func (c *continuous) runJob(ctx context.Context, job *config.Continuous) (dayChanged bool) {
	c.log().Debug(job.Name, "Processing job")

	now := c.now()
	files := fillDatesAt(job.Files, now)
	outfile := fillDatesAt(job.Outfile, now)
	servers := jobServersStr(job.Servers, c.cfg.Server.SSHBindAddress)

	args := config.Args{
		ConnectionsPerCPU: config.DefaultConnectionsPerCPU,
		Discovery:         job.Discovery,
		ServersStr:        servers,
		What:              files,
		Mode:              omode.TailClient,
		SSHArgs: config.SSHArgs{
			NoAuthKey: true,
			UserName:  config.ContinuousUser,
		},
	}

	args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(job.Name))
	args.QueryStr = fmt.Sprintf("%s outfile %s", job.Query, outfile)
	// A client whose discovery yields no server fails here with
	// clients.ErrNoServers, so the job writes no outfile (it used to
	// overwrite it with empty results) and runs again at the next retry.
	client, err := c.newMaprClient(args, clients.NonCumulativeMode)
	if err != nil {
		logCreateFailure(c.log(), job.Name, err)
		return false
	}

	jobCtx, cancel := context.WithCancel(ctx)
	var watcher sync.WaitGroup
	var restart atomic.Bool
	defer func() {
		cancel()
		watcher.Wait()
		dayChanged = restart.Load() && ctx.Err() == nil
	}()
	if job.RestartOnDayChange {
		watcher.Add(1)
		go func() {
			defer watcher.Done()
			// The watcher compares with the day the files and the outfile
			// were filled in for (now), not with the time it starts at:
			// should midnight pass while the client is created (discovery,
			// DNS), the job would otherwise follow the old day's files for
			// the whole new day.
			if c.dayChangeWatcher(jobCtx, now) {
				c.log().Info(fmt.Sprintf("Canceling job %s due to day change", job.Name))
				restart.Store(true)
				cancel()
			}
		}()
	}

	c.log().Info(fmt.Sprintf("Starting job %s", job.Name))
	status := client.Start(jobCtx, make(chan string))
	logMessage := fmt.Sprintf("Job exited with status %d", status)
	if status != 0 {
		c.log().Warn(logMessage)
		return
	}
	c.log().Info(logMessage)
	return
}

// waitForDayChange returns true once the calendar day differs from that of
// startTime, and false when ctx ends first.
func (c *continuous) waitForDayChange(ctx context.Context, startTime time.Time) bool {
	if !sameCalendarDay(c.now(), startTime) {
		return true
	}
	tickCh, stop := c.newTicker(time.Second)
	defer stop()
	for {
		select {
		case <-tickCh:
			if !sameCalendarDay(c.now(), startTime) {
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
}

func sameCalendarDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
