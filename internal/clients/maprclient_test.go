package clients

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/omode"
)

func TestMaprClientCommitSessionSpecResetsSharedState(t *testing.T) {
	query := mustMaprClientQuery(t, "select count(status) from stats group by status")
	args := config.Args{Mode: omode.MapClient, QueryStr: query.RawQuery, RegexStr: maprRegexForQuery(query)}
	client := &MaprClient{
		baseClient: baseClient{
			mu:   newBaseClientMu(),
			Args: args,
		},
		session: maprclient.NewSessionState(query, logging.NopLogger{}),
		mode:    DefaultMode,
	}

	initial := client.session.Snapshot()
	initial.GlobalGroup.Update(func(group *mapr.GroupSet) {
		set := group.GetSet("ERROR")
		set.Samples = 1
		set.FValues[query.Select[0].FieldStorage] = 1
	})
	if changed, ok := client.session.CommitRenderedResult(initial.Generation, "old-result"); !ok || !changed {
		t.Fatalf("CommitRenderedResult() = changed:%v ok:%v, want changed and ok", changed, ok)
	}

	spec := SessionSpec{
		Query: "select count(status) from warnings group by status",
	}
	if err := client.commitSessionSpec(spec, 4); err != nil {
		t.Fatalf("commitSessionSpec() error = %v", err)
	}

	updated := client.session.Snapshot()
	if updated.Generation != 4 {
		t.Fatalf("generation = %d, want 4", updated.Generation)
	}
	if updated.Query == nil || updated.Query.RawQuery != spec.Query {
		t.Fatalf("unexpected query after commit: %#v", updated.Query)
	}
	if !updated.GlobalGroup.IsEmpty() {
		t.Fatalf("expected committed global group to be reset")
	}
	if updated.LastResult != "" {
		t.Fatalf("last result = %q, want empty", updated.LastResult)
	}
	// The args are left to applyInteractiveReload, which stores them under
	// the state lock; the commit must not write them unlocked.
	if client.QueryStr != args.QueryStr || client.RegexStr != args.RegexStr {
		t.Fatalf("commitSessionSpec() changed the args: query %q regex %q, want %q and %q",
			client.QueryStr, client.RegexStr, args.QueryStr, args.RegexStr)
	}
}

// TestMaprClientReloadDoesNotRaceWithSnapshots runs interactive query
// reloads, whose profile commit is commitSessionSpec, while another goroutine
// snapshots the args as a reconnect does. Run with -race: the commit must not
// write c.Args, which the snapshot reads under the state lock. The stored args
// are the reloaded, normalized ones.
func TestMaprClientReloadDoesNotRaceWithSnapshots(t *testing.T) {
	queries := []string{
		"select count(status) from stats group by status",
		"select count(status) from warnings group by status",
	}
	query := mustMaprClientQuery(t, queries[0])
	spec := SessionSpec{Mode: omode.MapClient, Files: []string{"/var/log/app.log"},
		Query: queries[0], Regex: maprRegexForQuery(query)}
	conn := &interactiveReloadConnector{server: "srv1", supported: true,
		committedSpec: spec, liveSpec: spec, generation: 1}
	client := &MaprClient{
		baseClient: baseClient{
			mu:          newBaseClientMu(),
			Args:        config.Args{Mode: omode.MapClient, What: "/var/log/app.log", QueryStr: queries[0]},
			sessionSpec: spec,
			connections: []connectors.Connector{conn},
		},
		session: maprclient.NewSessionState(query, logging.NopLogger{}),
		mode:    DefaultMode,
	}
	client.profile.commit = client.commitSessionSpec

	stop := make(chan struct{})
	snapshots := make(chan struct{})
	go func() {
		defer close(snapshots)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = client.snapshotConnectionState()
			}
		}
	}()

	var last config.Args
	for i := 0; i < 50; i++ {
		command, err := parseInteractiveCommand(client.Args, ":reload --query '"+queries[(i+1)%2]+"'", nil)
		if err != nil {
			t.Fatalf("parseInteractiveCommand() error = %v", err)
		}
		if err := client.applyInteractiveReload(command.next, command.spec); err != nil {
			t.Fatalf("applyInteractiveReload() error = %v", err)
		}
		last = command.next
	}
	close(stop)
	<-snapshots

	args, _ := client.snapshotConnectionState()
	if args.QueryStr != last.QueryStr || args.RegexStr != `\|MAPREDUCE:STATS\|` {
		t.Fatalf("stored args = query %q regex %q, want %q with the STATS filter",
			args.QueryStr, args.RegexStr, last.QueryStr)
	}
}

func TestMaprClientCommitSessionSpecRejectsMissingQuery(t *testing.T) {
	query := mustMaprClientQuery(t, "select count(status) from stats group by status")
	client := &MaprClient{
		baseClient: baseClient{
			mu:   newBaseClientMu(),
			Args: config.Args{Mode: omode.MapClient},
		},
		session: maprclient.NewSessionState(query, logging.NopLogger{}),
		mode:    DefaultMode,
	}

	if err := client.commitSessionSpec(SessionSpec{}, 2); err == nil {
		t.Fatalf("expected commitSessionSpec() to reject empty query")
	}
}

func TestMaprClientReportDelayUsesRampUpAndSteadyIntervals(t *testing.T) {
	query := mustMaprClientQuery(t, "select count(status) from stats group by status interval 8")
	client := &MaprClient{}

	if delay := client.reportDelay(query, true); delay != 4*time.Second {
		t.Fatalf("ramp-up delay = %v, want 4s", delay)
	}
	if delay := client.reportDelay(query, false); delay != 8*time.Second {
		t.Fatalf("steady delay = %v, want 8s", delay)
	}
}

func TestMaprClientOutfileEmptyIntervalPolicy(t *testing.T) {
	type reportStep struct {
		generation uint64
		rowCount   int
		final      bool
		want       string
	}

	tests := []struct {
		name       string
		mode       MaprClientMode
		appendMode bool
		initial    string
		steps      []reportStep
	}{
		{
			name:  "initial empty creates header",
			mode:  NonCumulativeMode,
			steps: []reportStep{{want: "count(foo)\n"}},
		},
		{
			name:    "initial empty clears stale outfile",
			mode:    NonCumulativeMode,
			initial: "stale result\n",
			steps:   []reportStep{{want: "count(foo)\n"}},
		},
		{
			name: "empty interval preserves current generation result",
			mode: NonCumulativeMode,
			steps: []reportStep{
				{rowCount: 7, want: "count(foo)\n7\n"},
				{want: "count(foo)\n7\n"},
				{final: true, want: "count(foo)\n7\n"},
			},
		},
		{
			name: "new generation empty clears previous generation result",
			mode: NonCumulativeMode,
			steps: []reportStep{
				{rowCount: 7, want: "count(foo)\n7\n"},
				{generation: 1, want: "count(foo)\n"},
			},
		},
		{
			name:       "append empty creates header",
			mode:       NonCumulativeMode,
			appendMode: true,
			steps:      []reportStep{{want: "count(foo)\n"}},
		},
		{
			name:       "append empty preserves existing outfile",
			mode:       NonCumulativeMode,
			appendMode: true,
			initial:    "previous result\n",
			steps:      []reportStep{{want: "previous result\n"}},
		},
		{
			name:       "append consumes each non-cumulative interval once",
			mode:       NonCumulativeMode,
			appendMode: true,
			steps: []reportStep{
				{rowCount: 7, want: "count(foo)\n7\n"},
				{want: "count(foo)\n7\n"},
			},
		},
		{
			name:    "cumulative empty replaces stale outfile",
			mode:    CumulativeMode,
			initial: "stale result\n",
			steps:   []reportStep{{final: true, want: "count(foo)\n"}},
		},
		{
			name:    "cumulative final publishes current result",
			mode:    CumulativeMode,
			initial: "stale result\n",
			steps:   []reportStep{{rowCount: 7, final: true, want: "count(foo)\n7\n"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outfile := filepath.Join(t.TempDir(), "result.csv")
			if tt.initial != "" {
				if err := os.WriteFile(outfile, []byte(tt.initial), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}

			query := outfileTestQuery(t, outfile, tt.appendMode)
			client := newOutfileTestClient(query, tt.mode)
			var generation uint64
			for i, step := range tt.steps {
				if step.generation != generation {
					if _, err := client.session.CommitQuery(query.RawQuery, step.generation); err != nil {
						t.Fatalf("step %d CommitQuery() error = %v", i, err)
					}
					generation = step.generation
				}

				snapshot := client.session.Snapshot()
				if step.rowCount > 0 {
					mergeOutfileTestRow(t, snapshot, step.rowCount)
				}
				if err := client.writeResultsToOutfile(snapshot, step.final); err != nil {
					t.Fatalf("step %d writeResultsToOutfile() error = %v", i, err)
				}
				got, err := os.ReadFile(outfile)
				if err != nil {
					t.Fatalf("step %d ReadFile() error = %v", i, err)
				}
				if string(got) != step.want {
					t.Fatalf("step %d outfile = %q, want %q", i, got, step.want)
				}
			}
		})
	}
}

func TestMaprClientFailedNonEmptyWriteDoesNotPreserveFollowingEmptyInterval(t *testing.T) {
	root := t.TempDir()
	outfileDir := filepath.Join(root, "missing")
	outfile := filepath.Join(outfileDir, "result.csv")
	query := outfileTestQuery(t, outfile, false)
	client := newOutfileTestClient(query, NonCumulativeMode)

	snapshot := client.session.Snapshot()
	mergeOutfileTestRow(t, snapshot, 7)
	if err := client.writeResultsToOutfile(snapshot, false); err == nil {
		t.Fatal("writeResultsToOutfile() error = nil, want missing-directory error")
	}
	if err := os.Mkdir(outfileDir, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	if err := client.writeResultsToOutfile(client.session.Snapshot(), false); err != nil {
		t.Fatalf("empty writeResultsToOutfile() error = %v", err)
	}
	got, err := os.ReadFile(outfile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if want := "count(foo)\n"; string(got) != want {
		t.Fatalf("outfile = %q, want %q", got, want)
	}
}

func TestMaprClientRejectsRetainedSnapshotAfterNewGenerationPublication(t *testing.T) {
	outfile := filepath.Join(t.TempDir(), "result.csv")
	query := outfileTestQuery(t, outfile, false)
	client := newOutfileTestClient(query, NonCumulativeMode)

	retained := client.session.Snapshot()
	mergeOutfileTestRow(t, retained, 7)
	if _, err := client.session.CommitQuery(query.RawQuery, 1); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	current := client.session.Snapshot()
	mergeOutfileTestRow(t, current, 9)
	if err := client.writeResultsToOutfile(current, false); err != nil {
		t.Fatalf("current writeResultsToOutfile() error = %v", err)
	}

	if err := client.writeResultsToOutfile(retained, false); err != nil {
		t.Fatalf("stale writeResultsToOutfile() error = %v", err)
	}
	got, err := os.ReadFile(outfile)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if want := "count(foo)\n9\n"; string(got) != want {
		t.Fatalf("outfile after stale report = %q, want %q", got, want)
	}
	if retained.GlobalGroup.IsEmpty() {
		t.Fatal("rejected stale snapshot was consumed")
	}

	client.outfileState.mu.Lock()
	generation := client.outfileState.generation
	client.outfileState.mu.Unlock()
	if generation != current.Generation {
		t.Fatalf("outfile state generation = %d, want current generation %d", generation, current.Generation)
	}
}

func newOutfileTestClient(query *mapr.Query, mode MaprClientMode) *MaprClient {
	return &MaprClient{
		baseClient: baseClient{loggers: LoggerDependencies{}.normalized()},
		session:    maprclient.NewSessionState(query, logging.NopLogger{}),
		mode:       mode,
	}
}

func outfileTestQuery(t *testing.T, outfile string, appendMode bool) *mapr.Query {
	t.Helper()

	appendClause := ""
	if appendMode {
		appendClause = "append "
	}
	return mustMaprClientQuery(t, fmt.Sprintf("from STATS select count(foo) outfile %s%q", appendClause, outfile))
}

func mergeOutfileTestRow(t *testing.T, snapshot maprclient.SessionSnapshot, count int) {
	t.Helper()

	set := mapr.NewAggregateSet()
	set.Samples = 1
	set.FValues["count(foo)"] = float64(count)
	var err error
	snapshot.GlobalGroup.Update(func(group *mapr.GroupSet) {
		err = group.GetSet("host-a").Merge(snapshot.Query, set)
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
}

func mustMaprClientQuery(t *testing.T, queryStr string) *mapr.Query {
	t.Helper()

	query, err := mapr.NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery(%q) error = %v", queryStr, err)
	}
	return query
}

// TestWarnUnknownQueryVariables verifies the plan-time diagnostic is written to
// the client's stderr for an unknown $-variable (the ys0 footgun) and stays
// silent for a valid query, so users are not trained to ignore warnings.
func TestWarnUnknownQueryVariables(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantSubst string // empty means: expect no output
	}{
		{
			name:      "ys0 footgun warns",
			query:     "from STATS select $service,sum($bytes) group by $service",
			wantSubst: "$service is not a known variable",
		},
		{
			name:  "valid query does not warn",
			query: "from STATS select $hostname,max($goroutines) group by $hostname",
		},
		{
			name:  "barewords do not warn",
			query: "from STATS select service,sum(bytes) group by service",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query := mustMaprClientQuery(t, tc.query)
			var buf bytes.Buffer
			warnUnknownQueryVariables(&buf, query, logging.NopLogger{})
			out := buf.String()
			if tc.wantSubst == "" {
				if out != "" {
					t.Errorf("expected no warning, got: %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantSubst) {
				t.Errorf("expected warning containing %q, got: %q", tc.wantSubst, out)
			}
			if strings.Count(out, "warning:") != strings.Count(out, "\n") {
				t.Errorf("warnings should be one per line, got: %q", out)
			}
		})
	}
}
