package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

type continuous struct {
}

func newContinuous() *continuous {
	return &continuous{}
}

func (c *continuous) start(ctx context.Context) {
	logger.Info("Starting continuous job runner after 10s")
	time.Sleep(time.Second * 10)

	c.runJobs(ctx)
}

func (c *continuous) runJobs(ctx context.Context) {
	for _, job := range config.Server.Continuous {
		if !job.Enable {
			logger.Debug(job.Name, "Not running job as not enabled")
			continue
		}

		go func(job config.Continuous) {
			c.runJob(ctx, job)
			for {
				select {
				// Retry after a minute
				case <-time.After(time.Minute):
					c.runJob(ctx, job)
				case <-ctx.Done():
					return
				}
			}
		}(job)
	}
}

func (c *continuous) runJob(ctx context.Context, job config.Continuous) {
	logger.Debug(job.Name, "Processing job")

	files := fillDates(job.Files)
	outfile := fillDates(job.Outfile)

	servers := strings.Join(job.Servers, ",")
	if servers == "" {
		servers = config.Server.SSHBindAddress
	}

	args := clients.Args{
		ConnectionsPerCPU: 10,
		Discovery:         job.Discovery,
		ServersStr:        servers,
		What:              files,
		Mode:              omode.TailClient,
		UserName:          config.ContinuousUser,
	}

	args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(job.Name))

	query := fmt.Sprintf("%s outfile %s", job.Query, outfile)
	client, err := clients.NewMaprClient(args, query, clients.NonCumulativeMode)
	if err != nil {
		logger.Error(fmt.Sprintf("Unable to create job %s", job.Name), err)
		return
	}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if job.RestartOnDayChange {
		go func() {
			if c.waitForDayChange(ctx) {
				logger.Info(fmt.Sprintf("Canceling job %s due to day change", job.Name))
				cancel()
			}
		}()
	}

	logger.Info(fmt.Sprintf("Starting job %s", job.Name))
	status := client.Start(jobCtx, make(chan string))
	logMessage := fmt.Sprintf("Job exited with status %d", status)

	if status != 0 {
		logger.Warn(logMessage)
		return
	}
	logger.Info(logMessage)
}

func (c *continuous) waitForDayChange(ctx context.Context) bool {
	startTime := time.Now()
	for {
		select {
		case <-time.After(time.Second):
			if time.Now().Day() != startTime.Day() {
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
}
