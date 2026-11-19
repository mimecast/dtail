package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

const testBindAddress = "127.0.0.1"

// TestJobServersStr checks that job Servers are trimmed and empty entries
// dropped before they are joined, and that a list with no server left means
// this dserver (its SSHBindAddress), as doc/server-configuration.md says.
func TestJobServersStr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		servers []string
		want    string
	}{
		{name: "nil servers", servers: nil, want: testBindAddress},
		{name: "empty entry", servers: []string{""}, want: testBindAddress},
		{name: "only blank entries", servers: []string{" ", "\t", ""}, want: testBindAddress},
		{name: "trimmed entries", servers: []string{" alpha:2222 ", "beta\t"}, want: "alpha:2222,beta"},
		{name: "blank entries dropped", servers: []string{"alpha", " ", "", "beta"}, want: "alpha,beta"},
		{name: "server list file", servers: []string{" servers.txt "}, want: "servers.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := jobServersStr(tt.servers, testBindAddress); got != tt.want {
				t.Fatalf("jobServersStr(%q) = %q, want %q", tt.servers, got, tt.want)
			}
		})
	}
}

// TestJobServersStrDialsLoopbackForAnUnspecifiedBindAddress checks the
// fallback for a job with empty Servers on a dserver listening on every
// address: the bind address "" (which used to give the --servers value "" and
// fail every run) and the unspecified addresses map to the loopback address of
// their family. The client adds the configured SSHPort to the address. Any
// other bind address is used as it is.
func TestJobServersStrDialsLoopbackForAnUnspecifiedBindAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		bindAddress string
		want        string
	}{
		{bindAddress: "", want: "127.0.0.1"},
		{bindAddress: "0.0.0.0", want: "127.0.0.1"},
		{bindAddress: "::ffff:0.0.0.0", want: "127.0.0.1"},
		{bindAddress: "::", want: "::1"},
		{bindAddress: "192.0.2.10", want: "192.0.2.10"},
		{bindAddress: "::1", want: "::1"},
		{bindAddress: "dserver.example", want: "dserver.example"},
	}

	for _, tt := range tests {
		if got := jobServersStr([]string{" "}, tt.bindAddress); got != tt.want {
			t.Errorf("jobServersStr with bind address %q = %q, want %q", tt.bindAddress, got, tt.want)
		}
	}
}

// TestJobsOnAnEmptyBindAddressDialThisDServer checks that the client of a
// job with empty Servers on a dserver with the bind address "" connects to
// the loopback address at the configured SSHPort, instead of failing to
// parse the server address "".
func TestJobsOnAnEmptyBindAddressDialThisDServer(t *testing.T) {
	t.Parallel()

	const port = 2345
	host, gotPort, err := connectors.ParseServerAddress(jobServersStr(nil, ""), port)
	if err != nil || host != "127.0.0.1" || gotPort != port {
		t.Fatalf("job server = %q:%d (%v), want 127.0.0.1:%d", host, gotPort, err, port)
	}
}

// TestJobsWithBlankServersRunOnThisDServer checks that a scheduled and a
// continuous job whose Servers are only blank entries connect to this
// dserver instead of failing on the server value " , ".
func TestJobsWithBlankServersRunOnThisDServer(t *testing.T) {
	t.Parallel()

	cfg := config.RuntimeConfig{Server: &config.ServerConfig{SSHBindAddress: testBindAddress}}
	blank := []string{" ", ""}

	s := newScheduler(cfg, jobTestLoggers)
	var scheduledArgs config.Args
	s.newMaprClient = func(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
		scheduledArgs = args
		return immediateBackgroundClient{}, nil
	}
	scheduled := config.Scheduled{}
	scheduled.Name = "scheduled-blank-servers"
	scheduled.Query = "select count(*)"
	scheduled.Outfile = filepath.Join(t.TempDir(), "result")
	scheduled.Servers = blank
	runSingleJob(s, &scheduled)
	if scheduledArgs.ServersStr != testBindAddress {
		t.Fatalf("scheduled ServersStr = %q, want %q", scheduledArgs.ServersStr, testBindAddress)
	}

	c := newContinuous(cfg, jobTestLoggers)
	var continuousArgs config.Args
	c.newMaprClient = func(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
		continuousArgs = args
		return immediateBackgroundClient{}, nil
	}
	continuous := config.Continuous{}
	continuous.Name = "continuous-blank-servers"
	continuous.Query = "select count(*)"
	continuous.Servers = blank
	c.runJob(context.Background(), &continuous)
	if continuousArgs.ServersStr != testBindAddress {
		t.Fatalf("continuous ServersStr = %q, want %q", continuousArgs.ServersStr, testBindAddress)
	}
}

// TestDynamicServerListIgnoresWhitespaceAroundTheFilePath checks that the
// scheduler, like discovery, treats "servers.txt " as the server list file
// servers.txt, whose servers it must discover again on every wave.
func TestDynamicServerListIgnoresWhitespaceAroundTheFilePath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "servers.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o600); err != nil {
		t.Fatalf("write server list file: %v", err)
	}
	for _, servers := range []string{path, path + " ", " \t" + path + "\t"} {
		if !dynamicServerList(config.Args{ServersStr: servers}) {
			t.Fatalf("dynamicServerList(%q) = false, want true", servers)
		}
	}
	if dynamicServerList(config.Args{ServersStr: "alpha, beta"}) {
		t.Fatal("dynamicServerList of a comma list = true, want false")
	}
	// The FILE module name is normalized like discovery.New does it, with
	// strings.ToUpper, so "fıle" (dotless ı) selects FILE in both.
	for _, method := range []string{"file", "FILE", "fıle"} {
		if !dynamicServerList(config.Args{Discovery: method, ServersStr: "alpha"}) {
			t.Fatalf("dynamicServerList with Discovery %q = false, want true", method)
		}
	}
}

// TestSchedulerBacksOffAJobWithNoServer runs a scheduled job with a real
// mapreduce client whose server list file names no server (only comment and
// blank lines). Creating the client fails with "no server to connect to",
// which must be a failed run like any other: no outfile, the failure logged
// and the job backed off, so the scheduler does not mistake it for success.
// Both the creation failure and the failed run are logged at WARN, not at
// ERROR (see logCreateFailure).
func TestSchedulerBacksOffAJobWithNoServer(t *testing.T) {
	dir := t.TempDir()
	serverFile := filepath.Join(dir, "servers.txt")
	if err := os.WriteFile(serverFile, []byte("# no servers yet\n\n"), 0o600); err != nil {
		t.Fatalf("write server list file: %v", err)
	}
	outfile := filepath.Join(dir, "result.csv")

	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
	job := config.Scheduled{}
	job.Name = "noserver"
	job.Enable = true
	job.TimeRange = [2]int{0, 24}
	job.Servers = []string{serverFile}
	job.Files = filepath.Join(dir, "app.log")
	job.Query = "from STATS select count($line) group by $hostname"
	job.Outfile = outfile
	cfg.Server.Schedule = []config.Scheduled{job}

	s := newScheduler(cfg, jobTestLoggers)
	logger := &levelLogger{}
	s.logger = logger
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	var createErr error
	s.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		args.KnownHostsPath = filepath.Join(dir, "known_hosts")
		args.TrustAllHosts = true
		client, err := clients.NewMaprClient(args, cfg, mode, jobTestLoggers)
		if err != nil {
			createErr = err
			return nil, err
		}
		return client, nil
	}

	s.runJobs(context.Background())
	if createErr == nil || !strings.Contains(createErr.Error(), "no server to connect to") {
		t.Fatalf("client creation error = %v, want no server to connect to", createErr)
	}
	if _, err := os.Stat(outfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed run left %s (stat error %v)", outfile, err)
	}
	wantLog := "WARN Job noserver failed and wrote no outfile " + outfile + " (failure 1 in a row), it runs again " +
		"from about 2026-09-22 01:01:00"
	if !slices.Contains(logger.lines, wantLog) {
		t.Fatalf("log = %q, want %q", logger.lines, wantLog)
	}
	wantCreate := "WARN Unable to create job noserver"
	if !slices.ContainsFunc(logger.lines, func(line string) bool {
		return strings.HasPrefix(line, wantCreate) && strings.Contains(line, "no server to connect to")
	}) {
		t.Fatalf("log = %q, want a line %q ... no server to connect to", logger.lines, wantCreate)
	}
	for _, line := range logger.lines {
		if strings.HasPrefix(line, "ERROR ") {
			t.Fatalf("log has an ERROR line %q, want WARN only", line)
		}
	}
}

// TestSchedulerBacksOffAJobWithDiscoveryOptions runs a scheduled job with a
// real mapreduce client, the old documented Discovery "comma:server1,server2"
// and empty Servers. The COMMA module takes no options, so creating the
// client fails with "takes no options" instead of the job silently querying
// this dserver (its empty Servers) and writing a wrong outfile that the
// scheduler would then skip for the rest of its period. The failure must be
// a failed run like any other: no outfile, the creation failure logged at
// ERROR (it is a configuration error, not clients.ErrNoServers) and the job
// backed off.
func TestSchedulerBacksOffAJobWithDiscoveryOptions(t *testing.T) {
	dir := t.TempDir()
	outfile := filepath.Join(dir, "result.csv")

	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
	job := config.Scheduled{}
	job.Name = "commaoptions"
	job.Enable = true
	job.TimeRange = [2]int{0, 24}
	job.Discovery = "comma:server1,server2"
	job.Files = filepath.Join(dir, "app.log")
	job.Query = "from STATS select count($line) group by $hostname"
	job.Outfile = outfile
	cfg.Server.Schedule = []config.Scheduled{job}

	s := newScheduler(cfg, jobTestLoggers)
	logger := &levelLogger{}
	s.logger = logger
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	var createErr error
	s.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		args.KnownHostsPath = filepath.Join(dir, "known_hosts")
		args.TrustAllHosts = true
		client, err := clients.NewMaprClient(args, cfg, mode, jobTestLoggers)
		if err != nil {
			createErr = err
			return nil, err
		}
		return client, nil
	}

	s.runJobs(context.Background())
	if createErr == nil || !strings.Contains(createErr.Error(), "takes no options") {
		t.Fatalf("client creation error = %v, want takes no options", createErr)
	}
	if _, err := os.Stat(outfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed run left %s (stat error %v)", outfile, err)
	}
	wantCreate := "ERROR Unable to create job commaoptions"
	if !slices.ContainsFunc(logger.lines, func(line string) bool {
		return strings.HasPrefix(line, wantCreate) && strings.Contains(line, "takes no options")
	}) {
		t.Fatalf("log = %q, want a line %q ... takes no options", logger.lines, wantCreate)
	}
	wantBackoff := "Job commaoptions failed and wrote no outfile " + outfile + " (failure 1 in a row), " +
		"it runs again from about 2026-09-22 01:01:00"
	if !slices.ContainsFunc(logger.lines, func(line string) bool {
		return strings.HasSuffix(line, wantBackoff)
	}) {
		t.Fatalf("log = %q, want a line ending in %q", logger.lines, wantBackoff)
	}
}

// TestLogCreateFailureLevels checks that a client creation failure because
// discovery found no server (clients.ErrNoServers, also when wrapped) is
// logged at WARN and any other failure at ERROR.
func TestLogCreateFailureLevels(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "wrapped no servers",
			err:  fmt.Errorf("unable to create client: %w", clients.ErrNoServers),
			want: "WARN Unable to create job alpha",
		},
		{
			name: "other error",
			err:  errors.New("parse server address"),
			want: "ERROR Unable to create job alpha",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &levelLogger{}
			logCreateFailure(logger, "alpha", tt.err)
			if len(logger.lines) != 1 || !strings.HasPrefix(logger.lines[0], tt.want) ||
				!strings.Contains(logger.lines[0], tt.err.Error()) {
				t.Fatalf("log = %q, want one line %q ... %q", logger.lines, tt.want, tt.err)
			}
		})
	}
}

// levelLogger records the WARN and ERROR lines it is given, each prefixed
// with its level.
type levelLogger struct {
	logging.NopLogger
	mu    sync.Mutex
	lines []string
}

func (l *levelLogger) Warn(args ...any) string { return l.record("WARN", args) }

func (l *levelLogger) Error(args ...any) string { return l.record("ERROR", args) }

func (l *levelLogger) record(level string, args []any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprint(args...))
	return ""
}

// TestContinuousJobWithNoServerWritesNoOutfile runs a continuous job with a
// real mapreduce client whose server list file names no server (only comment
// and blank lines). Creating the client fails with "no server to connect
// to": the job writes no outfile (it used to overwrite the outfile with empty
// results) and logs the failure at WARN; the retry loop runs it again a
// minute later.
func TestContinuousJobWithNoServerWritesNoOutfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	serverFile := filepath.Join(dir, "servers.txt")
	if err := os.WriteFile(serverFile, []byte("# no servers yet\n\n"), 0o600); err != nil {
		t.Fatalf("write server list file: %v", err)
	}
	outfile := filepath.Join(dir, "result.csv")
	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
	job := config.Continuous{}
	job.Name = "noserver"
	job.Servers = []string{serverFile}
	job.Files = filepath.Join(dir, "app.log")
	job.Query = "from STATS select count($line) group by $hostname"
	job.Outfile = outfile

	c := newContinuous(cfg, jobTestLoggers)
	logger := &levelLogger{}
	c.logger = logger
	c.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		args.KnownHostsPath = filepath.Join(dir, "known_hosts")
		return clients.NewMaprClient(args, cfg, mode, jobTestLoggers)
	}

	c.runJob(context.Background(), &job)
	if _, err := os.Stat(outfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job with no server left %s (stat error %v)", outfile, err)
	}
	want := "WARN Unable to create job noserver"
	if len(logger.lines) != 1 || !strings.HasPrefix(logger.lines[0], want) ||
		!strings.Contains(logger.lines[0], "no server to connect to") {
		t.Fatalf("log = %q, want one line %q ... no server to connect to", logger.lines, want)
	}
}
