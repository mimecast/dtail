package clients

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/omode"
)

const switchTestQuery = "from STATS select count($line) group by $hostname"

// newSwitchTestClient returns the switchable client of interactive dtail
// (as NewInteractiveTailClient builds it, without connecting) following
// lines with args, with two fake connections whose sessions are committed at
// generation 4.
func newSwitchTestClient(t *testing.T, args config.Args) (*MaprClient, []*interactiveReloadConnector) {
	t.Helper()
	args.Mode = omode.TailClient
	args.InteractiveQuery = true
	spec := NewSessionSpec(args)
	conns := []*interactiveReloadConnector{
		{server: "srv1", committedSpec: spec, liveSpec: spec, generation: 4},
		{server: "srv2", committedSpec: spec, liveSpec: spec, generation: 4},
	}
	c := &MaprClient{
		baseClient: baseClient{
			mu:          newBaseClientMu(),
			Args:        args,
			sessionSpec: spec,
			connections: []connectors.Connector{conns[0], conns[1]},
		},
		session: maprclient.NewSessionState(nil, logging.NopLogger{}),
	}
	c.loggers = LoggerDependencies{}.normalized()
	c.runtime = newClientRuntimeBoundary(clientTestRuntimeConfig(), c.loggers)
	c.profile = clientProfile{commit: c.commitSessionSpec, switchWorkload: true, retry: true,
		expectQuery: c.session.ExpectAggregates}
	c.prober = c.newInteractiveProber()
	return c, conns
}

func tailSwitchArgs() config.Args {
	return config.Args{
		What:        "/var/log/app.log",
		RegexStr:    "ERROR",
		RegexInvert: true,
		LContext:    lcontext.LContext{BeforeContext: 1, AfterContext: 2, MaxCount: 3},
	}
}

// TestSwitchWorkloadTailToQueryAndBack switches the session of interactive
// dtail from following lines to a query and back: every connection gets the
// query's spec (its table's regex, never inverted, no context lines or
// --max), the client aggregates for the query, and the switch back restores
// the tail read's regex, --invert, context lines and --max and clears the
// client-side query state.
func TestSwitchWorkloadTailToQueryAndBack(t *testing.T) {
	c, conns := newSwitchTestClient(t, tailSwitchArgs())
	tailSpec := conns[0].committedSpec

	if _, err := c.switchToQuery(switchTestQuery); err != nil {
		t.Fatalf("switchToQuery() error = %v", err)
	}
	for _, conn := range conns {
		spec := conn.committedSpec
		if spec.Query != switchTestQuery || spec.Regex != `\|MAPREDUCE:STATS\|` || spec.RegexInvert ||
			spec.Options != "" || spec.Mode != omode.TailClient || conn.generation != 5 {
			t.Fatalf("%s query spec = %#v at generation %d", conn.server, spec, conn.generation)
		}
	}
	snapshot := c.session.Snapshot()
	if snapshot.Query == nil || snapshot.Query.RawQuery != switchTestQuery || snapshot.Generation != 5 ||
		snapshot.GlobalGroup == nil {
		t.Fatalf("session after switch to query = %#v", snapshot)
	}
	if args, _ := c.snapshotConnectionState(); args.QueryStr != switchTestQuery || args.MaxCount != 0 {
		t.Fatalf("args after switch to query = %#v", args)
	}

	if _, err := c.switchToTail(""); err != nil {
		t.Fatalf("switchToTail() error = %v", err)
	}
	for _, conn := range conns {
		if spec := conn.committedSpec; spec.Query != "" || spec.Regex != tailSpec.Regex ||
			!spec.RegexInvert || spec.Options != tailSpec.Options || conn.generation != 6 {
			t.Fatalf("%s tail spec = %#v at generation %d, want %#v", conn.server, spec, conn.generation, tailSpec)
		}
	}
	if snapshot := c.session.Snapshot(); snapshot.Query != nil || snapshot.GlobalGroup != nil ||
		snapshot.Generation != 6 {
		t.Fatalf("session after switch to tail = %#v, want no query at generation 6", snapshot)
	}
	args, spec := c.snapshotConnectionState()
	if args.QueryStr != "" || args.RegexStr != "ERROR" || !args.RegexInvert || args.MaxCount != 3 ||
		args.BeforeContext != 1 || args.AfterContext != 2 || spec.Query != "" {
		t.Fatalf("args after switch to tail = %#v spec %#v", args, spec)
	}
}

// TestSwitchToTailWithRegex switches from a query to following lines
// matching a new regex: the regex replaces the tail read's and clears its
// --invert, context lines and --max are kept. While following lines a regex
// replaces the regex the same way.
func TestSwitchToTailWithRegex(t *testing.T) {
	c, conns := newSwitchTestClient(t, tailSwitchArgs())
	if _, err := c.switchToQuery(switchTestQuery); err != nil {
		t.Fatalf("switchToQuery() error = %v", err)
	}
	if _, err := c.switchToTail("WARN"); err != nil {
		t.Fatalf("switchToTail(WARN) error = %v", err)
	}
	spec := conns[0].committedSpec
	if spec.Query != "" || spec.Regex != "WARN" || spec.RegexInvert || !strings.Contains(spec.Options, "max=3") {
		t.Fatalf("tail spec = %#v, want regex WARN not inverted with max=3", spec)
	}
	if _, err := c.switchToTail("FATAL"); err != nil {
		t.Fatalf("switchToTail(FATAL) while tailing error = %v", err)
	}
	if spec := conns[0].committedSpec; spec.Regex != "FATAL" || spec.Query != "" {
		t.Fatalf("tail spec = %#v, want regex FATAL", spec)
	}
}

// TestSwitchToQueryReplacesQuery switches from one query to another: the
// tail filter set aside by the first switch is kept for the way back.
func TestSwitchToQueryReplacesQuery(t *testing.T) {
	c, conns := newSwitchTestClient(t, tailSwitchArgs())
	const other = "from STATS select max(currentConnections)"
	for _, query := range []string{switchTestQuery, other} {
		if _, err := c.switchToQuery(query); err != nil {
			t.Fatalf("switchToQuery(%q) error = %v", query, err)
		}
	}
	if conns[0].committedSpec.Query != other || c.session.Snapshot().Query.RawQuery != other {
		t.Fatalf("spec %#v, want query %q", conns[0].committedSpec, other)
	}
	if _, err := c.switchToTail(""); err != nil {
		t.Fatalf("switchToTail() error = %v", err)
	}
	if spec := conns[0].committedSpec; spec.Regex != "ERROR" || !spec.RegexInvert {
		t.Fatalf("tail spec = %#v, want the first tail read's filter", spec)
	}
}

// TestSwitchWorkloadRejectsAndKeepsWorkload covers the switches that are
// rejected: they leave every connection and the client state as they were.
func TestSwitchWorkloadRejectsAndKeepsWorkload(t *testing.T) {
	for _, test := range []struct {
		name    string
		switchF func(*MaprClient) error
		want    error
		wantMsg string
	}{
		{name: "blank query", want: errBlankQuery,
			switchF: func(c *MaprClient) error { _, err := c.switchToQuery(" \t"); return err }},
		{name: "invalid query", wantMsg: "invalid workload",
			switchF: func(c *MaprClient) error { _, err := c.switchToQuery("select from"); return err }},
		{name: "tail while tailing", want: errAlreadyTailing,
			switchF: func(c *MaprClient) error { _, err := c.switchToTail(""); return err }},
		{name: "invalid regex", wantMsg: "invalid workload",
			switchF: func(c *MaprClient) error { _, err := c.switchToTail("("); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, conns := newSwitchTestClient(t, tailSwitchArgs())
			before, beforeSpec := c.snapshotConnectionState()
			err := test.switchF(c)
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) ||
				!strings.Contains(err.Error(), test.wantMsg) {
				t.Fatalf("error = %v, want %v %q", err, test.want, test.wantMsg)
			}
			for _, conn := range conns {
				if conn.applyCount != 0 || conn.generation != 4 {
					t.Fatalf("%s was updated: applies %d generation %d", conn.server, conn.applyCount, conn.generation)
				}
			}
			after, afterSpec := c.snapshotConnectionState()
			if after.RegexStr != before.RegexStr || after.QueryStr != before.QueryStr ||
				afterSpec.Regex != beforeSpec.Regex || c.session.Snapshot().Query != nil {
				t.Fatalf("client state changed: %#v %#v", after, afterSpec)
			}
		})
	}
}

// TestSwitchWorkloadUnsupportedOutsideInteractiveDTail rejects a switch in
// every client that cannot hold both workloads: dmap (always a query), a
// MapReduce client of a non-interactive dtail, and dgrep (never a query).
func TestSwitchWorkloadUnsupportedOutsideInteractiveDTail(t *testing.T) {
	dmap, dmapConns := newSwitchTestClient(t, tailSwitchArgs())
	dmap.profile.switchWorkload = false
	dmap.Mode = omode.MapClient
	dgrep, dgrepConns := newSwitchTestClient(t, tailSwitchArgs())
	dgrep.profile.switchWorkload = false
	dgrep.Mode = omode.GrepClient
	plain, plainConns := newSwitchTestClient(t, tailSwitchArgs())
	plain.InteractiveQuery = false

	for name, c := range map[string]*MaprClient{"dmap": dmap, "dgrep": dgrep, "non-interactive": plain} {
		if _, err := c.switchToQuery(switchTestQuery); !errors.Is(err, errWorkloadSwitchUnsupported) {
			t.Fatalf("%s switchToQuery() error = %v, want %v", name, err, errWorkloadSwitchUnsupported)
		}
		if _, err := c.switchToTail("WARN"); !errors.Is(err, errWorkloadSwitchUnsupported) {
			t.Fatalf("%s switchToTail() error = %v, want %v", name, err, errWorkloadSwitchUnsupported)
		}
	}
	for _, conns := range [][]*interactiveReloadConnector{dmapConns, dgrepConns, plainConns} {
		if conns[0].applyCount != 0 {
			t.Fatal("a rejected switch updated a connection")
		}
	}
}

// TestSwitchWorkloadRollsBackFailedUpdate fails the update of the second
// connection: the first is rolled back to the tail read and the client keeps
// following lines without a query.
func TestSwitchWorkloadRollsBackFailedUpdate(t *testing.T) {
	c, conns := newSwitchTestClient(t, tailSwitchArgs())
	tailSpec := conns[0].committedSpec
	conns[1].applyErr = connectors.ErrUnexpectedSessionAck
	if _, err := c.switchToQuery(switchTestQuery); err == nil {
		t.Fatal("switchToQuery() succeeded with a failing connection")
	}
	if spec := conns[0].committedSpec; spec.Query != "" || spec.Regex != tailSpec.Regex {
		t.Fatalf("srv1 not rolled back: %#v", spec)
	}
	if args, _ := c.snapshotConnectionState(); args.QueryStr != "" || c.session.Snapshot().Query != nil {
		t.Fatalf("client switched despite the failure: %#v", args)
	}
	if c.savedTailFilter().defined {
		t.Fatal("failed switch stored a tail filter")
	}
	if c.session.AggregatesExpected() {
		t.Fatal("failed switch left the handlers expecting aggregates")
	}
}

// TestMaprClientCommitWithoutQuery commits a spec without a query: the
// switchable client drops its query state, any other MaprClient rejects it.
func TestMaprClientCommitWithoutQuery(t *testing.T) {
	c, _ := newSwitchTestClient(t, tailSwitchArgs())
	if err := c.commitSessionSpec(SessionSpec{Query: switchTestQuery}, 5); err != nil {
		t.Fatalf("commit query: %v", err)
	}
	if err := c.commitSessionSpec(SessionSpec{}, 6); err != nil {
		t.Fatalf("commit without query: %v", err)
	}
	if snapshot := c.session.Snapshot(); snapshot.Query != nil || snapshot.Generation != 6 {
		t.Fatalf("session = %#v, want no query at generation 6", snapshot)
	}
	c.profile.switchWorkload = false
	if err := c.commitSessionSpec(SessionSpec{}, 7); err == nil {
		t.Fatal("non-switchable client committed a spec without a query")
	}
}

// TestMaprClientFinishWhileTailing ends a switchable client that follows
// lines: no result is reported and no failure warned about, although a
// server reported one.
func TestMaprClientFinishWhileTailing(t *testing.T) {
	c, _ := newSwitchTestClient(t, tailSwitchArgs())
	logger := &warnRecorder{}
	c.loggers.Client = logger
	c.connections = []connectors.Connector{&retryTestConnector{server: "srv1",
		handler: &statusTestHandler{outcome: handlers.SessionOutcome{Failures: []string{"command: rejected"}}}}}
	if status := c.finish(nil, 0); status != 0 {
		t.Fatalf("finish() = %d, want 0", status)
	}
	if got := logger.warnings(); len(got) != 0 {
		t.Fatalf("warnings = %q, want none", got)
	}
}

// TestMaprClientWarnsOnlyForFailuresOfTheQuery switches to a query after a
// server reported a failure of the tail read: only the failure reported
// after the switch is warned about at the end.
func TestMaprClientWarnsOnlyForFailuresOfTheQuery(t *testing.T) {
	c, _ := newSwitchTestClient(t, tailSwitchArgs())
	logger := &warnRecorder{}
	c.loggers.Client = logger
	handler := &statusTestHandler{outcome: handlers.SessionOutcome{Completed: true,
		Failures: []string{"command: rejected"}}}
	c.connections = []connectors.Connector{&retryTestConnector{server: "srv1", handler: handler}}
	if err := c.commitSessionSpec(SessionSpec{Query: switchTestQuery}, 5); err != nil {
		t.Fatalf("commit query: %v", err)
	}
	handler.outcome.Failures = append(handler.outcome.Failures, "map: incomplete result")

	c.finish(nil, 0)
	want := incompleteWarningPrefix + "|srv1: map: incomplete result"
	if got := incompleteWarnings(logger); len(got) != 1 || got[0] != want {
		t.Fatalf("warnings = %q, want [%q]", got, want)
	}
}

// TestNewInteractiveTailClient builds the client of interactive dtail: with
// --query its session starts with the query, the tail filter of the command
// line set aside for a switch to following lines; without one it follows
// lines and has no query state. A non-interactive dtail cannot have it.
func TestNewInteractiveTailClient(t *testing.T) {
	args := tailSwitchArgs()
	args.Serverless = true
	args.InteractiveQuery = true
	args.QueryStr = switchTestQuery
	loggers := LoggerDependencies{}.normalized()
	c, err := NewInteractiveTailClient(args, clientTestRuntimeConfig(), loggers)
	if err != nil {
		t.Fatalf("NewInteractiveTailClient() with query: %v", err)
	}
	if spec := c.sessionSpec; spec.Mode != omode.TailClient || spec.Query != switchTestQuery ||
		spec.Regex != `\|MAPREDUCE:STATS\|` || spec.RegexInvert || strings.Contains(spec.Options, "max") {
		t.Fatalf("query session spec = %#v", spec)
	}
	if saved := c.savedTailFilter(); !saved.defined || saved.regex != "ERROR" || !saved.invert || saved.max != 3 {
		t.Fatalf("saved tail filter = %#v", saved)
	}
	if !c.profile.switchWorkload || !c.profile.retry || c.session.Snapshot().Query == nil {
		t.Fatalf("profile %#v, query %v", c.profile, c.session.Snapshot().Query)
	}

	args.QueryStr = ""
	c, err = NewInteractiveTailClient(args, clientTestRuntimeConfig(), loggers)
	if err != nil {
		t.Fatalf("NewInteractiveTailClient() without query: %v", err)
	}
	if spec := c.sessionSpec; spec.Query != "" || spec.Regex != "ERROR" || !spec.RegexInvert ||
		!strings.Contains(spec.Options, "max=3") || c.session.Snapshot().Query != nil {
		t.Fatalf("tail session spec = %#v", spec)
	}

	args.InteractiveQuery = false
	if _, err := NewInteractiveTailClient(args, clientTestRuntimeConfig(), loggers); err == nil {
		t.Fatal("NewInteractiveTailClient() accepted a non-interactive dtail")
	}
}

// TestSwitchToQueryExpectsAggregatesAndNotesInvert switches to a query: the
// handlers expect aggregates from then on, and the answer notes that the
// --invert of the lines followed before is set aside; without --invert there
// is no note.
func TestSwitchToQueryExpectsAggregatesAndNotesInvert(t *testing.T) {
	for _, invert := range []bool{true, false} {
		args := tailSwitchArgs()
		args.RegexInvert = invert
		c, _ := newSwitchTestClient(t, args)
		if c.session.AggregatesExpected() {
			t.Fatal("aggregates expected while following lines")
		}
		outcome, err := c.switchToQuery(switchTestQuery)
		if err != nil {
			t.Fatalf("switchToQuery() error = %v", err)
		}
		if !c.session.AggregatesExpected() {
			t.Fatal("no aggregates expected with a committed query")
		}
		message := outcome.message(nil, true)
		if strings.Contains(message, invertSetAsideNote) != invert {
			t.Fatalf("invert %v: answer %q", invert, message)
		}
	}
}

// TestSwitchToTailPublishesLastResultOnce switches from a query with a
// result back to following lines: the result is printed once more as the
// query's last result, and nothing is reported after it.
func TestSwitchToTailPublishesLastResultOnce(t *testing.T) {
	c, _ := newSwitchTestClient(t, tailSwitchArgs())
	logger := newPausingTestLogger()
	c.loggers.Client = logger
	if _, err := c.switchToQuery("from STATS select count(foo) group by $hostname"); err != nil {
		t.Fatalf("switchToQuery() error = %v", err)
	}
	mergeOutfileTestRow(t, c.session.Snapshot(), 7)

	if _, err := c.switchToTail(""); err != nil {
		t.Fatalf("switchToTail() error = %v", err)
	}
	printed := strings.Join(logger.writtenLines(), "")
	if !strings.Contains(printed, "count(foo)") || !strings.Contains(printed, "7") {
		t.Fatalf("last result not printed on the switch back: %q", printed)
	}
	if err := c.reportResults(false); err != nil {
		t.Fatalf("reportResults() error = %v", err)
	}
	if status := c.finish(nil, 0); status != 0 {
		t.Fatalf("finish() = %d", status)
	}
	if after := strings.Join(logger.writtenLines(), ""); after != printed {
		t.Fatalf("reported after the switch back: %q", strings.TrimPrefix(after, printed))
	}
}

// debugCountingLogger counts the debug lines logged.
type debugCountingLogger struct {
	clientlog.NopLogger
	debugs atomic.Int64
}

func (l *debugCountingLogger) Debug(...any) string {
	l.debugs.Add(1)
	return ""
}

// TestMaprReporterIdleWithoutQuery runs the reporter of interactive dtail
// following lines: it logs nothing (it used to log two debug lines every
// second) until a query is committed, and then reports as usual.
func TestMaprReporterIdleWithoutQuery(t *testing.T) {
	c, _ := newSwitchTestClient(t, tailSwitchArgs())
	logger := &debugCountingLogger{}
	c.loggers.Client = logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.periodicReportResults(ctx)
	}()
	time.Sleep(1200 * time.Millisecond)
	if n := logger.debugs.Load(); n != 0 {
		t.Fatalf("reporter logged %d debug lines without a query", n)
	}
	if err := c.commitSessionSpec(SessionSpec{Query: switchTestQuery}, 5); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for logger.debugs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if logger.debugs.Load() == 0 {
		t.Fatal("reporter did not resume after a query was committed")
	}
}
