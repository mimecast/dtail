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
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

type scheduler struct {
}

func newScheduler() *scheduler {
	return &scheduler{}
}

func (s *scheduler) start(ctx context.Context) {
	dlog.Server.Info("Starting scheduled job runner after 10s")
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
	for _, job := range config.Server.Schedule {
		if !job.Enable {
			dlog.Server.Debug(job.Name, "Not running job as not enabled")
			continue
		}

		hour, err := strconv.Atoi(time.Now().Format("15"))
		if err != nil {
			dlog.Server.Error(job.Name, "Unable to create job", err)
			continue
		}

		if hour < job.TimeRange[0] || hour >= job.TimeRange[1] {
			dlog.Server.Debug(job.Name, "Not running job out of time range")
			continue
		}

		files := fillDates(job.Files)
		outfile := fillDates(job.Outfile)

		_, err = os.Stat(outfile)
		if !os.IsNotExist(err) {
			dlog.Server.Debug(job.Name, "Not running job as outfile already exists", outfile)
			continue
		}

		servers := strings.Join(job.Servers, ",")
		if servers == "" {
			servers = config.Server.SSHBindAddress
		}

		args := config.Args{
			ConnectionsPerCPU: config.DefaultConnectionsPerCPU,
			Discovery:         job.Discovery,
			ServersStr:        servers,
			What:              files,
			Mode:              omode.MapClient,
			UserName:          config.ScheduleUser,
		}

		args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(job.Name))

		query := fmt.Sprintf("%s outfile %s", job.Query, outfile)
		client, err := clients.NewMaprClient(args, query, clients.CumulativeMode)
		if err != nil {
			dlog.Server.Error(fmt.Sprintf("Unable to create job %s", job.Name), err)
			continue
		}

		jobCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		dlog.Server.Info(fmt.Sprintf("Starting job %s", job.Name))
		status := client.Start(jobCtx, make(chan string))
		logMessage := fmt.Sprintf("Job exited with status %d", status)

		if status != 0 {
			dlog.Server.Warn(logMessage)
			continue
		}
		dlog.Server.Info(logMessage)
	}
}
