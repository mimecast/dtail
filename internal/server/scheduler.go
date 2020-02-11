package server

import (
	"context"
	"fmt"
	"math/rand"
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

const authLength = 64
const authCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@$%^&*()_+[]"

type scheduler struct {
	authPayload string
}

func newScheduler() *scheduler {
	seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))

	b := make([]byte, authLength)
	for i := range b {
		b[i] = authCharset[seededRand.Intn(len(authCharset))]
	}

	return &scheduler{
		authPayload: string(b),
	}
}

func (s *scheduler) start(ctx context.Context) {
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

		args := clients.Args{
			ConnectionsPerCPU: 10,
			Discovery:         scheduled.Discovery,
			ServersStr:        scheduled.Servers,
			What:              files,
			Mode:              omode.MapClient,
			UserName:          config.ScheduledUser,
		}
		args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(s.authPayload))

		tmpOutfile := fmt.Sprintf("%s.tmp", outfile)
		query := fmt.Sprintf("%s outfile %s", scheduled.Query, tmpOutfile)

		client, err := clients.NewMaprClient(args, query)
		if err != nil {
			logger.Error(fmt.Sprintf("Unable to create scheduled job %s", scheduled.Name), err)
			continue
		}

		logger.Info(fmt.Sprintf("Starting scheduled job %s", scheduled.Name))
		status := client.Start(ctx)
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

func fillDates(str string) string {
	yyyesterday := time.Now().Add(3 * -24 * time.Hour).Format("20060102")
	str = strings.ReplaceAll(str, "$yyyesterday", yyyesterday)

	yyesterday := time.Now().Add(2 * -24 * time.Hour).Format("20060102")
	str = strings.ReplaceAll(str, "$yyesterday", yyesterday)

	yesterday := time.Now().Add(1 * -24 * time.Hour).Format("20060102")
	str = strings.ReplaceAll(str, "$yesterday", yesterday)

	today := time.Now().Format("20060102")
	str = strings.ReplaceAll(str, "$today", today)

	tomorrow := time.Now().Add(1 * 24 * time.Hour).Format("20060102")
	return strings.ReplaceAll(str, "$tomorrow", tomorrow)
}
