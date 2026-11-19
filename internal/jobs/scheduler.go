package jobs

import (
	"context"
	"fmt"
	"os"
	"strconv"
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
}

func newScheduler(cfg config.RuntimeConfig, loggers clients.LoggerDependencies, colorizers ...*brush.Brush) *scheduler {
	colorizer := firstColorizer(colorizers)
	return &scheduler{
		cfg:    cfg,
		logger: logging.OrNop(loggers.Server),
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

func (s *scheduler) runJobs(ctx context.Context) {
	for i := range s.cfg.Server.Schedule {
		job := &s.cfg.Server.Schedule[i]
		if !job.Enable {
			s.log().Debug(job.Name, "Not running job as not enabled")
			continue
		}
		hour, err := strconv.Atoi(time.Now().Format("15"))
		if err != nil {
			s.log().Error(job.Name, "Unable to create job", err)
			continue
		}
		if hour < job.TimeRange[0] || hour >= job.TimeRange[1] {
			s.log().Debug(job.Name, "Not running job out of time range")
			continue
		}
		s.runJob(ctx, job)
	}
}

func (s *scheduler) runJob(ctx context.Context, job *config.Scheduled) {
	files := fillDates(job.Files)
	outfile := fillDates(job.Outfile)

	_, err := os.Stat(outfile)
	if !os.IsNotExist(err) {
		s.log().Debug(job.Name, "Not running job as outfile already exists", outfile)
		return
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
	client, err := s.newMaprClient(args, clients.CumulativeMode)
	if err != nil {
		s.log().Error(fmt.Sprintf("Unable to create job %s", job.Name), err)
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.log().Info(fmt.Sprintf("Starting job %s", job.Name))
	status := client.Start(jobCtx, make(chan string))
	logMessage := fmt.Sprintf("Job exited with status %d", status)

	if status != 0 {
		s.log().Warn(logMessage)
		return
	}

	s.log().Info(logMessage)
}
