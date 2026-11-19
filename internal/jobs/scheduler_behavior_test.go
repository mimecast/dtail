package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

type statusBackgroundClient struct{ status int }

func (c statusBackgroundClient) Start(context.Context, <-chan string) int { return c.status }

func TestSchedulerRunJobsSkipsDisabledAndOutOfRangeJobs(t *testing.T) {
	t.Parallel()

	disabled := config.Scheduled{}
	disabled.Name = "disabled"
	disabled.Enable = false
	outOfRange := config.Scheduled{}
	outOfRange.Name = "out-of-range"
	outOfRange.Enable = true
	outOfRange.TimeRange = [2]int{25, 26}
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		Schedule: []config.Scheduled{disabled, outOfRange},
	}}, jobTestLoggers)
	s.newMaprClient = func(config.Args, clients.MaprClientMode) (backgroundClient, error) {
		t.Fatal("disabled/out-of-range job created a client")
		return nil, nil
	}
	s.runJobs(context.Background())
}

func TestSchedulerRunJobSkipAndFailurePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		factoryErr error
		outfile    func(*testing.T) string
		wantCalls  int
	}{
		{
			name: "existing output skips job",
			outfile: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "existing")
				if err := os.WriteFile(path, []byte("done"), 0o600); err != nil {
					t.Fatalf("write existing output: %v", err)
				}
				return path
			},
		},
		{name: "client factory error", factoryErr: errors.New("factory failed"), wantCalls: 1},
		{name: "nonzero client status", status: 2, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
				SSHBindAddress: "127.0.0.1",
			}}, jobTestLoggers)
			calls := 0
			s.newMaprClient = func(config.Args, clients.MaprClientMode) (backgroundClient, error) {
				calls++
				if tt.factoryErr != nil {
					return nil, tt.factoryErr
				}
				return statusBackgroundClient{status: tt.status}, nil
			}
			job := config.Scheduled{}
			job.Name = "job"
			job.Query = "from STATS select count(*)"
			job.Outfile = filepath.Join(t.TempDir(), "result")
			if tt.outfile != nil {
				job.Outfile = tt.outfile(t)
			}
			s.runJob(context.Background(), &job)
			if calls != tt.wantCalls {
				t.Fatalf("client factory calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}
