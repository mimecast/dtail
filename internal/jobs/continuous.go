package jobs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
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
	dayChangeWatcher func(context.Context) bool
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
			c.runJob(ctx, job)
			retryTicker := time.NewTicker(c.retryInterval)
			defer retryTicker.Stop()
			for {
				select {
				// Retry after the configured interval.
				case <-retryTicker.C:
					c.runJob(ctx, job)
				case <-ctx.Done():
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

func (c *continuous) runJob(ctx context.Context, job *config.Continuous) {
	c.log().Debug(job.Name, "Processing job")

	files := fillDates(job.Files)
	outfile := fillDates(job.Outfile)
	servers := strings.Join(job.Servers, ",")
	if servers == "" {
		servers = c.cfg.Server.SSHBindAddress
	}

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
	client, err := c.newMaprClient(args, clients.NonCumulativeMode)
	if err != nil {
		c.log().Error(fmt.Sprintf("Unable to create job %s", job.Name), err)
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	var watcher sync.WaitGroup
	defer func() {
		cancel()
		watcher.Wait()
	}()
	if job.RestartOnDayChange {
		watcher.Add(1)
		go func() {
			defer watcher.Done()
			if c.dayChangeWatcher(jobCtx) {
				c.log().Info(fmt.Sprintf("Canceling job %s due to day change", job.Name))
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
}

func (c *continuous) waitForDayChange(ctx context.Context) bool {
	startTime := c.now()
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
