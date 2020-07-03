package server

import (
	"context"
	"fmt"
	"os"
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

func (s *continuous) start(ctx context.Context) {
	// First run after just 10s!
	time.Sleep(time.Second * 10)
	s.runJobs(ctx)

	for {
		select {
		case <-time.After(time.Minute):
			s.runJobs(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (s *continuous) runJobs(ctx context.Context) {
	for _, job := range config.Server.Schedule {
		if !job.Enable {
			logger.Debug(job.Name, "Not running job as not enabled")
			continue
		}

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
			Mode:              omode.MapClient,
			UserName:          config.BackgroundUser,
		}

		args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(job.Name))

		tmpOutfile := fmt.Sprintf("%s.tmp", outfile)
		query := fmt.Sprintf("%s outfile %s", job.Query, tmpOutfile)

		client, err := clients.NewMaprClient(args, query, clients.NonCumulativeMode)
		if err != nil {
			logger.Error(fmt.Sprintf("Unable to create job job %s", job.Name), err)
			continue
		}

		jobCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		logger.Info(fmt.Sprintf("Starting job job %s", job.Name))
		status := client.Start(jobCtx)
		logMessage := fmt.Sprintf("Job exited with status %d", status)

		if err := os.Rename(tmpOutfile, outfile); err == nil {
			logger.Info(job.Name, fmt.Sprintf("Renamed %s to %s", tmpOutfile, outfile))
		}

		if status != 0 {
			logger.Warn(logMessage)
			continue
		}
		logger.Info(logMessage)
	}
}
