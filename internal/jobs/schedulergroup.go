package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/discovery"
)

// dueJob is a scheduled job that runs in the current scheduler run.
type dueJob struct {
	job     *config.Scheduled
	args    config.Args
	outfile string
	// rangeEnd is when the job's TimeRange that the run is in ends.
	rangeEnd time.Time
	// final is set for a run after the TimeRange of a failed run ended
	// (see jobBackoff): it writes what it could read.
	final bool
}

// jobGroupKey identifies the jobs that read the same files from the same
// servers: they run together and let dserver read each file once for all
// of them.
type jobGroupKey struct {
	files     string
	servers   string
	discovery string
}

func (d dueJob) groupKey() jobGroupKey {
	return jobGroupKey{files: d.args.What, servers: d.args.ServersStr, discovery: d.args.Discovery}
}

// pendingRun is a run the scheduler may start in its current run: a job run
// within the job's TimeRange (scheduledRun) or the final run of a failed job
// (finalRun).
type pendingRun interface {
	// name is the name of the run's job.
	name() string
	// evaluate returns the client arguments of the run at now, or why it is
	// not due.
	evaluate(s *scheduler, now time.Time) (dueJob, string)
	// footprint returns what the run reads and writes at now.
	footprint(now time.Time) jobFootprint
}

// scheduledRun is a run of job within its TimeRange.
type scheduledRun struct {
	job *config.Scheduled
}

func (r scheduledRun) name() string { return r.job.Name }

func (r scheduledRun) evaluate(s *scheduler, now time.Time) (dueJob, string) {
	return s.evaluate(r.job, now)
}

func (r scheduledRun) footprint(now time.Time) jobFootprint {
	return footprintAt(r.job, now)
}

// finalRun is the final run of a job whose runs failed (see jobBackoff),
// with the files and the outfile of its last failed run within its TimeRange.
type finalRun struct {
	state failedJob
}

func (r finalRun) name() string { return r.state.due.job.Name }

// evaluate returns the final run, or why it is not due: its outfile exists by
// now, and then the job's failures for it are forgotten.
func (r finalRun) evaluate(s *scheduler, _ time.Time) (dueJob, string) {
	if _, err := os.Stat(r.state.due.outfile); !os.IsNotExist(err) {
		s.backoff.forget(r.state)
		return dueJob{}, "Not running final run as outfile already exists: " + r.state.due.outfile
	}
	due := r.state.due
	due.final = true
	due.rangeEnd = r.state.rangeEnd
	return due, ""
}

func (r finalRun) footprint(time.Time) jobFootprint {
	return footprintOf(r.state.due.args.What, r.state.due.outfile)
}

// serversKey identifies the servers a job connects to: the server addresses
// as configured and the discovery module that turns them into a server list.
type serversKey struct {
	servers   string
	discovery string
}

// groupLimits memoises, for one scheduler run, what discovering static server
// lists and resolving server addresses found: the group limit of each static
// set of servers and whether an address reaches this dserver. File-based
// lists and their hostnames are rediscovered for each wave because either
// may change. Without the memo, one scheduler run
// discovers the servers of a job and resolves their names (see
// thisDServer.reaches, with its lookupTimeout) again for every group it forms,
// also for the jobs on other dservers that run one at a time.
//
// It is not safe for concurrent use. Only the scheduler's own goroutine
// reaches it, while it forms the next group; the goroutines that runGroup
// starts for the jobs of a group run no group formation and never touch it.
type groupLimits struct {
	limits  map[serversKey]int
	reaches map[string]bool
}

func newGroupLimits() *groupLimits {
	return &groupLimits{limits: make(map[serversKey]int), reaches: make(map[string]bool)}
}

// nextGroup evaluates the first pending run at the current time and, if it
// is due, groups it with the pending runs that are due at the same time and
// read the same files from the same servers with the same discovery. It
// returns the group, empty when the first run is not due, and the runs still
// pending, in their order; those are evaluated again when their group starts.
//
// The runs of a group run together, before the runs that stay pending. So
// that the same jobs run and write the same outfiles as when every run ran on
// its own in order, a run joins the group only if it does not conflict (see
// jobFootprint.conflicts) with a run of the group or with an earlier run that
// stays pending. A run that writes the outfile of such a run thus stays
// pending, runs after it and, as before, only if that run did not write the
// outfile.
//
// A group has at most groupLimit runs; the other runs that could join it stay
// pending and form the next groups, each a wave of at most groupLimit runs
// sharing reads among themselves. Once the group is full, the remaining runs
// stay pending without being evaluated here, as they are evaluated again when
// their own group starts anyway.
func (s *scheduler) nextGroup(ctx context.Context, pending []pendingRun, limits *groupLimits) (
	[]dueJob, []pendingRun) {

	now := s.now()
	first, reason := pending[0].evaluate(s, now)
	if reason != "" {
		s.log().Debug(pending[0].name(), reason)
		return nil, pending[1:]
	}
	group := []dueJob{first}
	if len(pending) == 1 {
		return group, nil
	}
	limit := s.groupLimit(ctx, first.args, limits)
	if limit <= 1 {
		return group, pending[1:]
	}
	key := first.groupKey()
	// before holds the footprints of the group's runs and of the earlier
	// runs that stay pending: the runs a later run must not conflict with to
	// join the group.
	before := []jobFootprint{pending[0].footprint(now)}
	var rest []pendingRun
	for i, run := range pending[1:] {
		if len(group) >= limit {
			rest = append(rest, pending[1+i:]...)
			break
		}
		footprint := run.footprint(now)
		due, reason := run.evaluate(s, now)
		if reason != "" || due.groupKey() != key || footprint.conflictsWithAny(before) {
			rest = append(rest, run)
		} else {
			group = append(group, due)
		}
		before = append(before, footprint)
	}
	return group, rest
}

// runPending runs pending, one group after another (see nextGroup), with the
// server lookups of one scheduler run memoised in limits.
func (s *scheduler) runPending(ctx context.Context, pending []pendingRun, limits *groupLimits) {
	for len(pending) > 0 {
		if ctx.Err() != nil {
			return
		}
		var group []dueJob
		group, pending = s.nextGroup(ctx, pending, limits)
		if len(group) > 0 {
			s.runGroup(ctx, group)
		}
	}
}

// groupLimit returns how many jobs connecting to the servers of args may run
// together. Static server lists use the memo of the current scheduler run.
// File-based lists are read again for each wave because clients also read
// them anew when they connect, and the file may change between waves.
//
// Jobs run together only when every server of args reaches the dserver
// running the scheduler (see thisDServer.reaches), whose MaxConnections and
// MaxConcurrentCats the scheduler knows, and when that dserver shares reads
// (Server.SharedReadsDisable is not set): without a shared read, running jobs
// together only adds load. At most a quarter of MaxConnections and at most
// MaxConcurrentCats, divided by the number of servers each job connects to,
// and at least one, run together then.
//
// Every job of a group opens an SSH connection of its own to each of its
// servers, and dserver counts connections still in their handshake against
// MaxConnections too, and refuses the others; a refused job fails and runs
// again only after its backoff (see jobBackoff). The quarter leaves the other
// connections to interactive users and continuous jobs. The cat slots bound
// the group as dserver shares a read among at most MaxConcurrentCats members
// (see handlers.NewReadHub), each holding a cat slot during it: the members
// beyond that read the file on their own, and queue for the same cat slots
// while holding their connection. Dividing by the number of servers keeps
// both bounds when several server names of a job reach this dserver. The
// limits of other dservers are unknown to the scheduler, and others may use
// up their connections: jobs on them, and jobs whose servers cannot be
// discovered, run one at a time, as the scheduler did before it grouped jobs.
func (s *scheduler) groupLimit(ctx context.Context, args config.Args, limits *groupLimits) int {
	if s.cfg.Server.SharedReadsDisable {
		return 1
	}
	key := serversKey{servers: args.ServersStr, discovery: args.Discovery}
	if !dynamicServerList(args) {
		if limit, ok := limits.limits[key]; ok {
			return limit
		}
		limit := s.discoverGroupLimit(ctx, args, limits)
		limits.limits[key] = limit
		return limit
	}
	// A FILE may keep the same hostname while its DNS answer changes. Use a
	// fresh reachability memo as well as a fresh server list for this wave.
	return s.discoverGroupLimit(ctx, args, newGroupLimits())
}

// dynamicServerList mirrors discovery's implicit file selection as well as
// its explicit FILE module. Checking the path on every wave also notices a
// file that appears after the first wave.
func dynamicServerList(args config.Args) bool {
	method, _, _ := strings.Cut(args.Discovery, ":")
	if strings.EqualFold(method, "file") {
		return true
	}
	if method != "" {
		return false
	}
	_, err := os.Stat(args.ServersStr)
	return err == nil
}

// discoverGroupLimit discovers the servers of args and returns the group
// limit of jobs connecting to them.
func (s *scheduler) discoverGroupLimit(ctx context.Context, args config.Args, limits *groupLimits) int {
	finder, err := discovery.New(args.Discovery, args.ServersStr, discovery.Shuffle, s.log())
	var servers []string
	if err == nil {
		servers, err = finder.ServerList()
	}
	if err != nil {
		s.log().Warn("Running jobs one at a time as their servers can not be discovered", err)
		return 1
	}
	if len(servers) == 0 {
		return 1
	}
	for _, server := range servers {
		if !s.reachesThisDServer(ctx, server, limits) {
			s.log().Debug("Running jobs one at a time as a server is not this dserver", server)
			return 1
		}
	}
	cats := max(1, s.cfg.Server.MaxConcurrentCats)
	return max(1, min(s.cfg.Server.MaxConnections/4, cats)/len(servers))
}

// reachesThisDServer reports whether server reaches this dserver (see
// thisDServer.reaches), looking it up once per scheduler run.
//
// A successful or failed lookup of a static server list is kept for this
// scheduler run, including its DNS answer. A transient DNS failure makes
// every group using that server sequential until the next run a minute
// later. FILE discovery passes a fresh memo for each wave because its
// contents and DNS answers may change between waves.
func (s *scheduler) reachesThisDServer(ctx context.Context, server string, limits *groupLimits) bool {
	if reaches, ok := limits.reaches[server]; ok {
		return reaches
	}
	reaches := s.thisDServer.reaches(ctx, server)
	limits.reaches[server] = reaches
	return reaches
}

// jobFootprint is what a scheduled job reads and writes at a time: the
// patterns of the files it reads and the files it writes, its outfile, the
// outfile's .query file and their temporary files, each as configured
// (absolute and cleaned) and with symbolic links resolved (see outfileKey).
type jobFootprint struct {
	reads  []string
	writes []string
}

func footprintAt(job *config.Scheduled, now time.Time) jobFootprint {
	return footprintOf(fillDatesAt(job.Files, now), fillDatesAt(job.Outfile, now))
}

// footprintOf returns the footprint of a job reading files, a comma separated
// list of patterns, and writing outfile, both with their dates filled in.
func footprintOf(files, outfile string) jobFootprint {
	var footprint jobFootprint
	for _, pattern := range strings.Split(files, ",") {
		if pattern = strings.TrimSpace(pattern); pattern == "" {
			continue
		}
		footprint.reads = append(footprint.reads, filePatterns(pattern)...)
	}
	for _, path := range []string{absPath(outfile), outfileKey(outfile)} {
		for _, suffix := range []string{"", ".tmp", ".query", ".query.tmp"} {
			footprint.writes = append(footprint.writes, path+suffix)
		}
	}
	return footprint
}

// filePatterns returns pattern absolute and cleaned, and with the symbolic
// links of its directory resolved.
func filePatterns(pattern string) []string {
	path := absPath(pattern)
	patterns := []string{path}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		patterns = append(patterns, filepath.Join(dir, filepath.Base(path)))
	}
	return patterns
}

// conflictsWithAny reports whether f conflicts with one of others.
func (f jobFootprint) conflictsWithAny(others []jobFootprint) bool {
	return slices.ContainsFunc(others, f.conflicts)
}

// conflicts reports whether the jobs of f and other may not run in another
// order than configured, nor together: they write a file in common, or one
// reads a file the other writes. Two jobs that do not conflict give the same
// results in any order.
func (f jobFootprint) conflicts(other jobFootprint) bool {
	for _, path := range f.writes {
		if slices.Contains(other.writes, path) {
			return true
		}
	}
	return readsAny(f.reads, other.writes) || readsAny(other.reads, f.writes)
}

// readsAny reports whether one of patterns matches one of paths. A malformed
// pattern matches every path.
func readsAny(patterns, paths []string) bool {
	for _, pattern := range patterns {
		for _, path := range paths {
			if matched, err := filepath.Match(pattern, path); matched || err != nil {
				return true
			}
		}
	}
	return false
}

func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// outfileKey returns the file an outfile path names: the absolute, cleaned
// path, with the symbolic links of the path, or of its directory while the
// outfile does not exist, resolved. Two jobs writing ./x.csv and x.csv write
// the same file.
func outfileKey(outfile string) string {
	path := absPath(outfile)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}
	return path
}

// runGroup runs the jobs of one group. A single job runs as before. The jobs
// of a larger group run concurrently and ask
// dserver, with a read share that is random for this run, to read their files
// once for the whole group; a dserver without shared reads ignores the share
// and every job reads on its own.
func (s *scheduler) runGroup(ctx context.Context, group []dueJob) {
	if len(group) == 1 {
		s.runDueJob(ctx, group[0])
		return
	}

	share, err := config.NewReadShare(len(group))
	if err != nil {
		s.log().Warn("Running job group without a read share", err)
	}
	names := make([]string, len(group))
	for i := range group {
		names[i] = group[i].job.Name
		group[i].args.ReadShare = share
	}
	kind := "job group"
	if group[0].final {
		kind = "final run group"
	}
	s.log().Info(fmt.Sprintf("Starting %s of %d jobs reading %s together", kind, len(group), group[0].args.What),
		"jobs="+strings.Join(names, ","))

	var wg sync.WaitGroup
	for _, due := range group {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runDueJob(ctx, due)
		}()
	}
	wg.Wait()
}
