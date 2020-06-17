package server

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

type scheduler struct {
}

func newScheduler() *scheduler {
	return &scheduler{}
}

func (s *scheduler) start(ctx context.Context) {
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

func (s *scheduler) runJobs(ctx context.Context) {
	for _, scheduled := range config.Server.Schedule {
		if !scheduled.Enable {
			logger.Debug(scheduled.Name, "Not running job as not enabled")
			continue
		}

		hour, err := strconv.Atoi(time.Now().Format("15"))
		if err != nil {
			logger.Error(scheduled.Name, "Unable to create scheduled job", err)
			continue
		}

		if hour < scheduled.TimeRange[0] || hour >= scheduled.TimeRange[1] {
			logger.Debug(scheduled.Name, "Not running job out of time range")
			continue
		}

		files := fillDates(scheduled.Files)
		outfile := fillDates(scheduled.Outfile)

		_, err = os.Stat(outfile)
		if !os.IsNotExist(err) {
			logger.Debug(scheduled.Name, "Not running job as outfile already exists", outfile)
			continue
		}

		servers := strings.Join(scheduled.Servers, ",")
		if servers == "" {
			servers = config.Server.SSHBindAddress
		}

		args := clients.Args{
			ConnectionsPerCPU: 10,
			Discovery:         scheduled.Discovery,
			ServersStr:        servers,
			What:              files,
			Mode:              omode.MapClient,
			UserName:          config.BackgroundUser,
		}

		args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(scheduled.Name))

		tmpOutfile := fmt.Sprintf("%s.tmp", outfile)
		query := fmt.Sprintf("%s outfile %s", scheduled.Query, tmpOutfile)

		client, err := clients.NewMaprClient(args, query)
		if err != nil {
			logger.Error(fmt.Sprintf("Unable to create scheduled job %s", scheduled.Name), err)
			continue
		}

		jobCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		logger.Info(fmt.Sprintf("Starting scheduled job %s", scheduled.Name))
		status := client.Start(jobCtx)
		logMessage := fmt.Sprintf("Job exited with status %d", status)

		if err := os.Rename(tmpOutfile, outfile); err == nil {
			logger.Info(scheduled.Name, fmt.Sprintf("Renamed %s to %s", tmpOutfile, outfile))
		}

		if status != 0 {
			logger.Warn(logMessage)
			continue
		}
		logger.Info(logMessage)
	}
}
