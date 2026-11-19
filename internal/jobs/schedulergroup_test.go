package jobs

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

// groupRecorder is a fake client factory that records what the scheduler
// runs. Clients of a job named in together wait until all of those run at
// the same time, so a scheduler that ran them one after another times out.
type groupRecorder struct {
	t        *testing.T
	together map[string]bool

	mu      sync.Mutex
	events  []string
	args    map[string]config.Args
	running int
	joined  chan struct{}
}

func newGroupRecorder(t *testing.T, together ...string) *groupRecorder {
	r := &groupRecorder{t: t, together: map[string]bool{}, args: map[string]config.Args{},
		joined: make(chan struct{})}
	for _, name := range together {
		r.together[name] = true
	}
	return r
}

func (r *groupRecorder) newClient(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
	// The job's query, the start of the client's query, is its name here.
	name, _, _ := strings.Cut(args.QueryStr, " ")
	r.mu.Lock()
	r.args[name] = args
	r.mu.Unlock()
	return recordedClient{recorder: r, name: name}, nil
}

type recordedClient struct {
	recorder *groupRecorder
	name     string
}

func (c recordedClient) Start(context.Context, <-chan string) int {
	r := c.recorder
	r.mu.Lock()
	r.events = append(r.events, "start "+c.name)
	if r.together[c.name] {
		r.running++
		if r.running == len(r.together) {
			close(r.joined)
		}
	}
	r.mu.Unlock()
	if r.together[c.name] {
		select {
		case <-r.joined:
		case <-time.After(10 * time.Second):
			r.t.Errorf("job %s did not run together with the other jobs of its group", c.name)
		}
	}
	r.mu.Lock()
	r.events = append(r.events, "end "+c.name)
	r.mu.Unlock()
	return 0
}

func scheduledJob(t *testing.T, name, files, outfile string, servers ...string) config.Scheduled {
	t.Helper()
	job := config.Scheduled{}
	job.Name = name
	job.Enable = true
	job.TimeRange = [2]int{0, 24}
	job.Files = files
	job.Servers = servers
	job.Query = name
	job.Outfile = outfile
	return job
}

func TestSchedulerRunsJobsOnTheSameFilesTogether(t *testing.T) {
	dir := t.TempDir()
	out := func(name string) string { return filepath.Join(dir, name) }
	disabled := scheduledJob(t, "disabled", "/var/log/a.log", out("disabled"))
	disabled.Enable = false
	jobs := []config.Scheduled{
		scheduledJob(t, "a1", "/var/log/a.log", out("a1")),
		scheduledJob(t, "b", "/var/log/b.log", out("b")),
		scheduledJob(t, "a2", "/var/log/a.log", out("a2")),
		disabled,
		scheduledJob(t, "a3", "/var/log/a.log", out("a3")),
		// Same files, but another server: another group.
		scheduledJob(t, "a-remote", "/var/log/a.log", out("a-remote"), "192.0.2.1:2222"),
		// Same group key and outfile as a1: runs on its own, after a1.
		scheduledJob(t, "a1-again", "/var/log/a.log", out("a1")),
	}
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 40, Schedule: jobs,
	}}, jobTestLoggers)
	recorder := newGroupRecorder(t, "a1", "a2", "a3")
	s.newMaprClient = recorder.newClient

	s.runJobs(context.Background())

	// The a group runs first, its jobs together; the other groups follow one
	// after another in the order of their first job.
	events := recorder.events
	if len(events) != 12 {
		t.Fatalf("events = %q, want 6 jobs run", events)
	}
	groupEvents := append([]string(nil), events[:6]...)
	for _, event := range groupEvents {
		switch event {
		case "start a1", "start a2", "start a3", "end a1", "end a2", "end a3":
		default:
			t.Fatalf("the a group did not run first, alone: %q", events)
		}
	}
	if want := []string{"start b", "end b", "start a-remote", "end a-remote", "start a1-again", "end a1-again"}; !reflect.DeepEqual(events[6:], want) {
		t.Errorf("events after the a group = %q, want %q", events[6:], want)
	}

	share := recorder.args["a1"].ReadShare
	if share.Members != 3 || share.Group == "" {
		t.Fatalf("a1 read share = %+v, want a group of 3", share)
	}
	for _, name := range []string{"a2", "a3"} {
		if got := recorder.args[name].ReadShare; got != share {
			t.Errorf("%s read share = %+v, want %+v", name, got, share)
		}
	}
	for _, name := range []string{"b", "a-remote", "a1-again"} {
		if got := recorder.args[name].ReadShare; !got.IsZero() {
			t.Errorf("single job %s got read share %+v", name, got)
		}
	}
}

func TestSchedulerReadShareIsRandomPerRun(t *testing.T) {
	dir := t.TempDir()
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 40,
		Schedule: []config.Scheduled{
			scheduledJob(t, "x1", "/var/log/x.log", filepath.Join(dir, "x1")),
			scheduledJob(t, "x2", "/var/log/x.log", filepath.Join(dir, "x2")),
		},
	}}, jobTestLoggers)

	var groups []string
	for range 2 {
		recorder := newGroupRecorder(t, "x1", "x2")
		s.newMaprClient = recorder.newClient
		s.runJobs(context.Background())
		groups = append(groups, recorder.args["x1"].ReadShare.Group)
	}
	if groups[0] == "" || groups[0] == groups[1] {
		t.Errorf("read share groups of two runs = %q, want two different IDs", groups)
	}
}

func TestSchedulerGroupSkipsJobsWithAnExistingOutfile(t *testing.T) {
	dir := t.TempDir()
	done := filepath.Join(dir, "done")
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 40,
		Schedule: []config.Scheduled{
			scheduledJob(t, "y1", "/var/log/y.log", filepath.Join(dir, "y1")),
			scheduledJob(t, "y2", "/var/log/y.log", done),
			scheduledJob(t, "y3", "/var/log/y.log", filepath.Join(dir, "y3")),
		},
	}}, jobTestLoggers)
	if err := os.WriteFile(done, []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := newGroupRecorder(t, "y1", "y3")
	s.newMaprClient = recorder.newClient
	s.runJobs(context.Background())

	if _, ran := recorder.args["y2"]; ran {
		t.Error("a job with an existing outfile ran")
	}
	if got := recorder.args["y1"].ReadShare.Members; got != 2 {
		t.Errorf("group members = %d, want 2: the skipped job does not count", got)
	}
}

// The cat slots bound a wave as the connections do: dserver shares a read
// among at most MaxConcurrentCats members (see handlers.NewReadHub), and the
// members beyond that queue for the same cat slots while holding their
// connections, so the scheduler does not start them with the group.
func TestSchedulerBoundsWavesByTheCatSlots(t *testing.T) {
	dir := t.TempDir()
	var schedule []config.Scheduled
	for i := range 5 {
		name := fmt.Sprintf("j%d", i)
		schedule = append(schedule, scheduledJob(t, name, "/a.log,/b.log", filepath.Join(dir, name)))
	}
	// A quarter of MaxConnections would allow 10 jobs per wave.
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 2, MaxConnections: 40, Schedule: schedule,
	}}, jobTestLoggers)
	recorder := &waveRecorder{t: t, shares: map[string]config.ReadShare{},
		started: map[string]int{}, joined: map[string]chan struct{}{}}
	s.newMaprClient = recorder.newClient
	s.runJobs(context.Background())

	recorder.assertWaves(t, len(schedule), []int{2, 2, 1})
	for i := range 4 {
		name := fmt.Sprintf("j%d", i)
		if got := recorder.shares[name].Members; got != 2 {
			t.Errorf("%s read share members = %d, want 2, the cat slots", name, got)
		}
	}
}

// waveRecorder is a fake client factory that records the read share of each
// job and the most jobs that ran at the same time. A job runs until every
// job of its read share started, so jobs sharing a read that did not run
// together time out.
type waveRecorder struct {
	t *testing.T

	mu         sync.Mutex
	shares     map[string]config.ReadShare
	started    map[string]int
	joined     map[string]chan struct{}
	running    int
	maxRunning int
}

func (r *waveRecorder) newClient(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
	name, _, _ := strings.Cut(args.QueryStr, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shares[name] = args.ReadShare
	return waveClient{recorder: r, share: args.ReadShare}, nil
}

type waveClient struct {
	recorder *waveRecorder
	share    config.ReadShare
}

func (c waveClient) Start(context.Context, <-chan string) int {
	r := c.recorder
	r.mu.Lock()
	r.running++
	r.maxRunning = max(r.maxRunning, r.running)
	var joined chan struct{}
	if !c.share.IsZero() {
		if r.joined[c.share.Group] == nil {
			r.joined[c.share.Group] = make(chan struct{})
		}
		joined = r.joined[c.share.Group]
		r.started[c.share.Group]++
		if r.started[c.share.Group] == c.share.Members {
			close(joined)
		}
	}
	r.mu.Unlock()
	if joined != nil {
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			r.t.Errorf("jobs of read share %+v did not run together", c.share)
		}
	}
	// Give a scheduler that ran more jobs at once the time to start them.
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	return 0
}

// A group larger than a quarter of MaxConnections or than MaxConcurrentCats,
// divided by the servers of its jobs, runs in waves of at most that many
// jobs, one wave after another; the jobs of each wave share a read among
// themselves. Jobs on another dserver, whose limits the scheduler does not
// know, and all jobs when shared reads are disabled run one at a time.
func TestSchedulerRunsLargeGroupsInBoundedWaves(t *testing.T) {
	tests := []struct {
		name           string
		maxConnections int
		// maxConcurrentCats sets Server.MaxConcurrentCats; 0 sets enough cat
		// slots for the connection bound to be the smaller one.
		maxConcurrentCats int
		servers           []string
		discovery         string
		// sharedReadsDisable sets Server.SharedReadsDisable.
		sharedReadsDisable bool
		jobs               int
		wantWaves          []int
	}{
		{name: "default config", maxConnections: 10, jobs: 12, wantWaves: []int{2, 2, 2, 2, 2, 2}},
		{name: "uneven last wave", maxConnections: 12, jobs: 7, wantWaves: []int{3, 3, 1}},
		{name: "this dserver named twice", maxConnections: 24, servers: []string{"127.0.0.1:2222", "127.0.0.1"},
			jobs: 7, wantWaves: []int{3, 3, 1}},
		{name: "same server twice", maxConnections: 24, servers: []string{"127.0.0.1:2222", "127.0.0.1:2222"},
			jobs: 7, wantWaves: []int{6, 1}},
		{name: "fewer than four connections", maxConnections: 3, jobs: 3, wantWaves: []int{1, 1, 1}},
		{name: "more servers than the bound", maxConnections: 8,
			servers: []string{"127.0.0.1", "127.0.0.1:2222", "[::ffff:127.0.0.1]:2222"}, jobs: 2,
			wantWaves: []int{1, 1}},
		{name: "servers not discoverable", maxConnections: 40, discovery: "nosuchmodule", jobs: 3,
			wantWaves: []int{1, 1, 1}},
		{name: "another dserver", maxConnections: 40, servers: []string{"192.0.2.1:2222"}, jobs: 3,
			wantWaves: []int{1, 1, 1}},
		{name: "this and another dserver", maxConnections: 40, servers: []string{"127.0.0.1", "192.0.2.1"},
			jobs: 3, wantWaves: []int{1, 1, 1}},
		{name: "another port of this host", maxConnections: 40, servers: []string{"127.0.0.1:2223"}, jobs: 3,
			wantWaves: []int{1, 1, 1}},
		{name: "shared reads disabled", maxConnections: 40, sharedReadsDisable: true, jobs: 3,
			wantWaves: []int{1, 1, 1}},
		{name: "fewer cat slots than connections", maxConnections: 40, maxConcurrentCats: 3, jobs: 7,
			wantWaves: []int{3, 3, 1}},
		{name: "cat slots divided by the servers", maxConnections: 40, maxConcurrentCats: 4,
			servers: []string{"127.0.0.1:2222", "127.0.0.1"}, jobs: 5, wantWaves: []int{2, 2, 1}},
		{name: "one cat slot", maxConnections: 40, maxConcurrentCats: 1, jobs: 3, wantWaves: []int{1, 1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var schedule []config.Scheduled
			for i := range tt.jobs {
				name := fmt.Sprintf("j%d", i)
				job := scheduledJob(t, name, "/a.log", filepath.Join(dir, name), tt.servers...)
				job.Discovery = tt.discovery
				schedule = append(schedule, job)
			}
			cats := tt.maxConcurrentCats
			if cats == 0 {
				cats = 100
			}
			s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
				SSHBindAddress: "127.0.0.1", MaxConcurrentCats: cats, MaxConnections: tt.maxConnections,
				SharedReadsDisable: tt.sharedReadsDisable, Schedule: schedule,
			}}, jobTestLoggers)
			recorder := &waveRecorder{t: t, shares: map[string]config.ReadShare{},
				started: map[string]int{}, joined: map[string]chan struct{}{}}
			s.newMaprClient = recorder.newClient
			s.runJobs(context.Background())

			recorder.assertWaves(t, tt.jobs, tt.wantWaves)
		})
	}
}

// The scheduler discovers the servers of its jobs and resolves their names
// once per scheduler run, not once per group it forms: the runs of one job
// list on the same servers share the lookup. A run that is the only one left
// pending needs no lookup, as it has nothing to group with, and the runs
// before the first due run of a scheduler run need none either: a run that is
// not due leaves the group formation before the servers are looked up.
func TestSchedulerLooksUpTheServersOncePerRun(t *testing.T) {
	tests := []struct {
		name string
		jobs int
		// notDue names the jobs whose outfile exists when the scheduler runs.
		notDue []string
		// wantWaves is checked when every job is due.
		wantWaves   []int
		wantStarted int
		wantLookups int
	}{
		{name: "three waves of two jobs", jobs: 6, wantWaves: []int{2, 2, 2}, wantStarted: 6,
			wantLookups: 1},
		{name: "a single job", jobs: 1, wantWaves: []int{1}, wantStarted: 1, wantLookups: 0},
		{name: "only the first job is due", jobs: 3, notDue: []string{"j1", "j2"}, wantStarted: 1,
			wantLookups: 1},
		{name: "only the middle job is due", jobs: 3, notDue: []string{"j0", "j2"}, wantStarted: 1,
			wantLookups: 1},
		// The two runs before it are not due and leave before the lookup, so
		// the due run is the only one left pending and needs none either.
		{name: "only the last job is due", jobs: 3, notDue: []string{"j0", "j1"}, wantStarted: 1,
			wantLookups: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var schedule []config.Scheduled
			for i := range tt.jobs {
				name := fmt.Sprintf("j%d", i)
				schedule = append(schedule,
					scheduledJob(t, name, "/a.log", filepath.Join(dir, name), "myhost"))
			}
			for _, name := range tt.notDue {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("done"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
				SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 2, MaxConnections: 40, Schedule: schedule,
			}}, jobTestLoggers)
			var mu sync.Mutex
			lookups := 0
			s.thisDServer.lookup = func(context.Context, string) ([]netip.Addr, error) {
				mu.Lock()
				defer mu.Unlock()
				lookups++
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			recorder := &waveRecorder{t: t, shares: map[string]config.ReadShare{},
				started: map[string]int{}, joined: map[string]chan struct{}{}}
			s.newMaprClient = recorder.newClient
			s.runJobs(context.Background())

			if len(tt.notDue) == 0 {
				recorder.assertWaves(t, tt.jobs, tt.wantWaves)
			}
			if got := len(recorder.shares); got != tt.wantStarted {
				t.Errorf("jobs started = %d, want %d", got, tt.wantStarted)
			}
			if lookups != tt.wantLookups {
				t.Errorf("server name lookups = %d, want %d", lookups, tt.wantLookups)
			}
		})
	}
}

// A FILE list is read again when the next wave forms, just as each job's
// client reads it again when it connects. Reusing the first wave's limit
// could start jobs together after the file points to another dserver.
func TestSchedulerRechecksFileDiscoveryBetweenWaves(t *testing.T) {
	for _, method := range []string{"", "file"} {
		t.Run("discovery="+method, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "servers")
			writeServers := func(content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeServers("127.0.0.1\n")
			s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
				SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 2, MaxConnections: 40,
			}}, jobTestLoggers)
			args := config.Args{Discovery: method, ServersStr: path}
			limits := newGroupLimits()
			if got := s.groupLimit(context.Background(), args, limits); got != 2 {
				t.Fatalf("first wave limit = %d, want 2", got)
			}
			writeServers("192.0.2.1\n")
			if got := s.groupLimit(context.Background(), args, limits); got != 1 {
				t.Errorf("remote server second wave limit = %d, want 1", got)
			}
			writeServers("127.0.0.1\n127.0.0.1:2222\n")
			if got := s.groupLimit(context.Background(), args, limits); got != 1 {
				t.Errorf("two local servers third wave limit = %d, want 1", got)
			}
			// Even when the file keeps the same hostname, its DNS answer may
			// change between waves. The reachability memo must be fresh too.
			writeServers("myhost\n")
			lookups := 0
			s.thisDServer.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
				if host != "myhost" {
					t.Errorf("looked up unexpected host %q", host)
				}
				lookups++
				if lookups == 1 {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
			}
			if got := s.groupLimit(context.Background(), args, limits); got != 2 {
				t.Errorf("local hostname wave limit = %d, want 2", got)
			}
			if got := s.groupLimit(context.Background(), args, limits); got != 1 {
				t.Errorf("remote hostname next wave limit = %d, want 1", got)
			}
			if lookups != 2 {
				t.Errorf("hostname lookups = %d, want one per wave", lookups)
			}
		})
	}
}

// failingClient is a client whose run fails.
type failingClient struct{}

func (failingClient) Start(context.Context, <-chan string) int { return 1 }

// The final runs of jobs on the same files that failed within their
// TimeRange run as their runs within the TimeRange did: together, in waves of
// at most a quarter of MaxConnections and at most MaxConcurrentCats, each
// wave sharing a read among its jobs, in the configured order.
func TestSchedulerRunsFinalRunsInBoundedWaves(t *testing.T) {
	dir := t.TempDir()
	var schedule []config.Scheduled
	for i := range 7 {
		name := fmt.Sprintf("j%d", i)
		job := scheduledJob(t, name, "/a.log", filepath.Join(dir, name))
		job.TimeRange = [2]int{1, 2}
		schedule = append(schedule, job)
	}
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 12, Schedule: schedule,
	}}, jobTestLoggers)
	now := time.Date(2026, 9, 22, 1, 30, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	recorder := &waveRecorder{t: t, shares: map[string]config.ReadShare{},
		started: map[string]int{}, joined: map[string]chan struct{}{}}
	var mu sync.Mutex
	var modes []clients.MaprClientMode
	s.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
		mu.Lock()
		modes = append(modes, mode)
		mu.Unlock()
		if mode == clients.ScheduledMode {
			return failingClient{}, nil
		}
		return recorder.newClient(args, mode)
	}

	s.runJobs(context.Background())
	if len(recorder.shares) != 0 {
		t.Fatalf("final runs ran within the TimeRange: %v", recorder.shares)
	}
	now = time.Date(2026, 9, 22, 2, 0, 0, 0, time.Local)
	s.runJobs(context.Background())

	recorder.assertWaves(t, len(schedule), []int{3, 3, 1})
	want := slices.Concat(slices.Repeat([]clients.MaprClientMode{clients.ScheduledMode}, 7),
		slices.Repeat([]clients.MaprClientMode{clients.ScheduledPartialMode}, 7))
	if !slices.Equal(modes, want) {
		t.Errorf("jobs ran with modes %v, want %v", modes, want)
	}
}

// assertWaves checks that the jobs j0 to j<jobs-1> ran in waves of
// wantWaves jobs, in the configured order: the runs of consecutive jobs with
// the same read share, all started together; a job running alone has none.
func (r *waveRecorder) assertWaves(t *testing.T, jobs int, wantWaves []int) {
	t.Helper()
	var waves []int
	prev := ""
	jobsOfShare := map[string]int{}
	for i := range jobs {
		share := r.shares[fmt.Sprintf("j%d", i)]
		switch {
		case share.IsZero():
			waves = append(waves, 1)
		case share.Group == prev:
			waves[len(waves)-1]++
		case jobsOfShare[share.Group] > 0:
			t.Fatalf("read share %+v of j%d is not in one wave", share, i)
		default:
			waves = append(waves, 1)
		}
		prev = share.Group
		if !share.IsZero() {
			jobsOfShare[share.Group]++
			if share.Members < 2 {
				t.Errorf("j%d has a read share of %d members", i, share.Members)
			}
		}
	}
	if !reflect.DeepEqual(waves, wantWaves) {
		t.Errorf("waves = %v, want %v", waves, wantWaves)
	}
	for i := range jobs {
		share := r.shares[fmt.Sprintf("j%d", i)]
		if !share.IsZero() && (share.Members != jobsOfShare[share.Group] || share.Members != r.started[share.Group]) {
			t.Errorf("j%d read share %+v: %d jobs have it, %d started", i, share,
				jobsOfShare[share.Group], r.started[share.Group])
		}
	}
	if want := slices.Max(wantWaves); r.maxRunning != want {
		t.Errorf("at most %d jobs ran at the same time, want %d", r.maxRunning, want)
	}
}

// outfileWriter is a fake client factory whose jobs write their outfile, as
// a MapReduce job does when it ends.
type outfileWriter struct {
	mu   sync.Mutex
	runs []string
}

func (w *outfileWriter) newClient(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
	name, _, _ := strings.Cut(args.QueryStr, " ")
	_, outfile, _ := strings.Cut(args.QueryStr, " outfile ")
	return outfileClient{writer: w, name: name, outfile: outfile}, nil
}

type outfileClient struct {
	writer  *outfileWriter
	name    string
	outfile string
}

func (c outfileClient) Start(context.Context, <-chan string) int {
	c.writer.mu.Lock()
	c.writer.runs = append(c.writer.runs, c.name)
	c.writer.mu.Unlock()
	if err := os.WriteFile(c.outfile, []byte(c.name), 0o600); err != nil {
		return 1
	}
	return 0
}

// j3 reads the files of j1 and could run in j1's group, but j2, an earlier
// job on other files, writes j3's outfile. Run one by one, j2 wrote x.csv and
// j3 was skipped; j3 must not run before j2 and write x.csv instead.
func TestSchedulerRunsNoJobBeforeAnEarlierJobOnItsOutfile(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
		SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 40,
		Schedule: []config.Scheduled{
			scheduledJob(t, "j1", "/var/log/a.log", "one.csv"),
			scheduledJob(t, "j2", "/var/log/b.log", "x.csv"),
			scheduledJob(t, "j3", "/var/log/a.log", "x.csv"),
		},
	}}, jobTestLoggers)
	writer := &outfileWriter{}
	s.newMaprClient = writer.newClient
	s.runJobs(context.Background())

	if got, _ := os.ReadFile("x.csv"); string(got) != "j2" {
		t.Errorf("x.csv written by %q, want j2", got)
	}
	if !reflect.DeepEqual(writer.runs, []string{"j1", "j2"}) {
		t.Errorf("jobs run = %q, want j1 and j2", writer.runs)
	}
}

func TestSchedulerSkipsAJobWhoseOutfileAnEarlierJobWrote(t *testing.T) {
	tests := []struct {
		name     string
		outfiles func(dir string) (string, string)
	}{
		{"same path", func(string) (string, string) { return "x.csv", "x.csv" }},
		{"relative spellings", func(string) (string, string) { return "./x.csv", "x.csv" }},
		{"absolute and relative", func(dir string) (string, string) {
			return filepath.Join(dir, "x.csv"), "x.csv"
		}},
		{"unclean path", func(string) (string, string) { return "sub/../x.csv", "x.csv" }},
		{"symlinked directory", func(dir string) (string, string) {
			return filepath.Join("link", "x.csv"), filepath.Join("real", "x.csv")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			for _, sub := range []string{"sub", "real"} {
				if err := os.Mkdir(sub, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("real", "link"); err != nil {
				t.Fatal(err)
			}
			first, second := tt.outfiles(dir)
			s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
				SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 40,
				Schedule: []config.Scheduled{
					scheduledJob(t, "first", "/var/log/x.log", first),
					scheduledJob(t, "second", "/var/log/x.log", second),
				},
			}}, jobTestLoggers)
			writer := &outfileWriter{}
			s.newMaprClient = writer.newClient
			s.runJobs(context.Background())

			if !reflect.DeepEqual(writer.runs, []string{"first"}) {
				t.Errorf("jobs run = %q, want only the first: the second one's outfile exists", writer.runs)
			}
		})
	}
}

// fakeClock is a scheduler clock that jobs can move forward.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// clockClient is a job that moves the clock to after while it runs.
type clockClient struct {
	clock *fakeClock
	after time.Time
}

func (c clockClient) Start(context.Context, <-chan string) int {
	c.clock.set(c.after)
	return 0
}

// A job is evaluated when its group starts, as when every job ran on its own
// right after the one before it, not when the scheduler run started.
func TestSchedulerEvaluatesAJobWhenItsGroupStarts(t *testing.T) {
	day := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local)
	tests := []struct {
		name      string
		start     time.Time
		after     time.Time
		timeRange [2]int
		wantRun   bool
		wantFiles string
	}{
		{"still in its time range", day.Add(10 * time.Hour), day.Add(10*time.Hour + time.Minute),
			[2]int{10, 11}, true, "/var/log/b-20260921.log"},
		{"out of its time range by then", day.Add(10*time.Hour + 59*time.Minute), day.Add(11 * time.Hour),
			[2]int{10, 11}, false, ""},
		{"the dates changed by then", day.Add(23*time.Hour + 59*time.Minute), day.Add(24 * time.Hour),
			[2]int{0, 24}, true, "/var/log/b-20260922.log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			b := scheduledJob(t, "b", "/var/log/b-$today.log", filepath.Join(dir, "b-$today"))
			b.TimeRange = tt.timeRange
			s := newScheduler(config.RuntimeConfig{Server: &config.ServerConfig{
				SSHBindAddress: "127.0.0.1", MaxConcurrentCats: 8, MaxConnections: 40,
				Schedule: []config.Scheduled{
					scheduledJob(t, "a", "/var/log/a.log", filepath.Join(dir, "a")),
					b,
				},
			}}, jobTestLoggers)
			clock := &fakeClock{now: tt.start}
			s.now = clock.Now
			ran := map[string]config.Args{}
			s.newMaprClient = func(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
				name, _, _ := strings.Cut(args.QueryStr, " ")
				ran[name] = args
				return clockClient{clock: clock, after: tt.after}, nil
			}
			s.runJobs(context.Background())

			if _, ok := ran["a"]; !ok {
				t.Fatal("job a did not run")
			}
			args, ok := ran["b"]
			if ok != tt.wantRun {
				t.Fatalf("job b ran = %v, want %v", ok, tt.wantRun)
			}
			if ok && args.What != tt.wantFiles {
				t.Errorf("job b read %q, want %q", args.What, tt.wantFiles)
			}
		})
	}
}
