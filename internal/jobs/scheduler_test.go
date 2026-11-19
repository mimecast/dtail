package jobs

import (
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

func TestSchedulerRunJobDisablesAuthKeyRegistration(t *testing.T) {

	s := newScheduler(config.RuntimeConfig{
		Server: &config.ServerConfig{SSHBindAddress: "127.0.0.1"},
	}, jobTestLoggers)
	var capturedArgs config.Args
	s.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		capturedArgs = args
		if mode != clients.ScheduledMode {
			t.Fatalf("Unexpected client mode: %v", mode)
		}
		return immediateBackgroundClient{}, nil
	}

	job := config.Scheduled{}
	job.Name = "scheduled-job"
	job.Query = "select count(*)"
	job.Outfile = filepath.Join(t.TempDir(), "result")
	runSingleJob(s, &job)

	if !capturedArgs.NoAuthKey {
		t.Fatal("Expected scheduled client to disable AUTHKEY registration")
	}
	if capturedArgs.UserName != config.ScheduleUser {
		t.Fatalf("Unexpected user name: %q", capturedArgs.UserName)
	}
}
