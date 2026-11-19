package jobs

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

// TestSchedulerRetriesAFailedJobAfterItsBackoff runs a scheduled job with
// real mapreduce clients. The first run cannot reach its server (connection
// refused): it must leave no outfile and no .query file. A scheduler run
// before its backoff ended skips it. The run a minute later reaches the data
// (in-process, serverless) and writes the result, and the run after that
// skips the job because its outfile exists.
func TestSchedulerRetriesAFailedJobAfterItsBackoff(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")
	lines := strings.Repeat("INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
		"currentConnections=0|lifetimeConnections=1\n", 3)
	if err := os.WriteFile(logFile, []byte(lines), 0o600); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	outfile := filepath.Join(dir, "result.csv")

	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
	job := config.Scheduled{}
	job.Name = "retried"
	job.Enable = true
	job.TimeRange = [2]int{0, 24}
	job.Servers = []string{closedTCPAddress(t)}
	job.Files = logFile
	job.Query = "from STATS select count($line),max(lifetimeConnections) group by $hostname"
	job.Outfile = outfile
	cfg.Server.Schedule = []config.Scheduled{job}

	s := newScheduler(cfg, jobTestLoggers)
	logger := &exitLogger{}
	s.logger = logger
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	runs := 0
	s.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		runs++
		args.KnownHostsPath = filepath.Join(dir, "known_hosts")
		// From the second run on the data is reachable.
		args.Serverless = runs > 1
		return clients.NewMaprClient(args, cfg, mode, jobTestLoggers)
	}

	s.runJobs(context.Background())
	if runs != 1 {
		t.Fatalf("first run created %d clients, want 1", runs)
	}
	for _, path := range []string{outfile, outfile + ".query", outfile + ".tmp", outfile + ".query.tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed run left %s (stat error %v)", path, err)
		}
	}
	wantLog := "Job retried failed and wrote no outfile " + outfile + " (failure 1 in a row), it runs again " +
		"from about 2026-09-22 01:01:00"
	if !slices.Contains(logger.lines, wantLog) {
		t.Fatalf("log = %q, want %q", logger.lines, wantLog)
	}

	now = now.Add(30 * time.Second)
	s.runJobs(context.Background())
	if runs != 1 {
		t.Fatalf("run within the backoff created %d clients in total, want 1", runs)
	}

	now = now.Add(30 * time.Second)
	s.runJobs(context.Background())
	if runs != 2 {
		t.Fatalf("second run created %d clients in total, want 2", runs)
	}
	assertJobFile(t, outfile, "count($line),max(lifetimeConnections)\n3,1.000000\n")
	assertJobFile(t, outfile+".query", job.Query+" outfile "+outfile)

	s.runJobs(context.Background())
	if runs != 2 {
		t.Fatalf("run after success created %d clients in total, want 2", runs)
	}
}

// TestSchedulerRetriesAJobWhoseFileDoesNotExistYet runs, with real
// (serverless) mapreduce clients, a scheduled job whose file of today does
// not exist yet. The read fails, so the job must leave no outfile, not even
// a header-only one; once the file exists, the next run after the backoff
// writes the result.
func TestSchedulerRetriesAJobWhoseFileDoesNotExistYet(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	outfile := filepath.Join(dir, "result.csv")

	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
	cfg.Server.ReadGlobRetryIntervalMs = 10
	job := config.Scheduled{}
	job.Name = "today"
	job.Enable = true
	job.TimeRange = [2]int{0, 24}
	job.Files = filepath.Join(dir, "app-$today.log")
	job.Query = "from STATS select count($line),max(lifetimeConnections) group by $hostname"
	job.Outfile = outfile
	cfg.Server.Schedule = []config.Scheduled{job}

	s := newScheduler(cfg, jobTestLoggers)
	logger := &exitLogger{}
	s.logger = logger
	s.now = func() time.Time { return now }
	runs := 0
	s.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		runs++
		args.Serverless = true
		return clients.NewMaprClient(args, cfg, mode, jobTestLoggers)
	}

	s.runJobs(context.Background())
	if runs != 1 {
		t.Fatalf("first run created %d clients, want 1", runs)
	}
	for _, path := range []string{outfile, outfile + ".query", outfile + ".tmp", outfile + ".query.tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("run without the file left %s (stat error %v)", path, err)
		}
	}
	if got := countLines(logger.lines, "Job today failed and wrote no outfile"); got != 1 {
		t.Fatalf("logged %d failures, want 1: %q", got, logger.lines)
	}

	lines := strings.Repeat("INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
		"currentConnections=0|lifetimeConnections=2\n", 4)
	if err := os.WriteFile(filepath.Join(dir, "app-20260922.log"), []byte(lines), 0o600); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	now = now.Add(time.Minute)
	s.runJobs(context.Background())
	if runs != 2 {
		t.Fatalf("run after the backoff created %d clients in total, want 2", runs)
	}
	assertJobFile(t, outfile, "count($line),max(lifetimeConnections)\n4,2.000000\n")
}

// closedTCPAddress returns a local address nothing listens on.
func closedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return address
}

func assertJobFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
