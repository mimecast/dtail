package server

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

const authLength = 64
const authCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@£$%^&*()_+[]"

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
		case <-time.After(time.Second * 10):
			s.runJobs(ctx)
			return
		case <-time.After(time.Minute):
			s.runJobs(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (s *scheduler) runJobs(ctx context.Context) {
	for _, scheduled := range config.Server.Schedule {
		args := clients.Args{
			ConnectionsPerCPU: scheduled.ConnectionsPerCPU,
			Discovery:         scheduled.Discovery,
			ServersStr:        scheduled.Servers,
			What:              scheduled.Files,
			Mode:              omode.MapClient,
			UserName:          config.ScheduledUser,
		}
		args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(s.authPayload))

		client, err := clients.NewMaprClient(args, scheduled.Query)
		if err != nil {
			logger.Error(fmt.Sprintf("Unable to create scheduled job %s", scheduled.Name), err)
			continue
		}

		logger.Info(fmt.Sprintf("Starting scheduled job %s", scheduled.Name))
		status := client.Start(ctx)
		logMessage := fmt.Sprintf("Scheduled job %s exited with status %d", scheduled.Name, status)
		if status != 0 {
			logger.Warn(logMessage)
			continue
		}
		logger.Info(logMessage)
	}
}
