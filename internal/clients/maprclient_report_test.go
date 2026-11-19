package clients

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/omode"
)

// tableCaptureLogger records when (fake time since start) a result table
// was printed through Raw, and the numbers of its rows.
type tableCaptureLogger struct {
	clientlog.NopLogger
	start time.Time
	// printTime is how long printing a table takes.
	printTime time.Duration
	mu        sync.Mutex
	at        []time.Duration
	values    [][]float64
}

func (l *tableCaptureLogger) Raw(message string) string {
	if !strings.Contains(message, "count(foo)") || !strings.Contains(message, "---") {
		return ""
	}
	var values []float64
	for _, field := range strings.Fields(message) {
		if value, err := strconv.ParseFloat(field, 64); err == nil {
			values = append(values, value)
		}
	}
	l.mu.Lock()
	l.at = append(l.at, time.Since(l.start))
	l.values = append(l.values, values)
	l.mu.Unlock()
	time.Sleep(l.printTime)
	return ""
}

func (l *tableCaptureLogger) tables() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.at...)
}

// table returns the time and the numbers of the i-th printed table.
func (l *tableCaptureLogger) table(i int) (time.Duration, []float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.at[i], l.values[i]
}

// newReportTestClient returns a printing (no outfile) MaprClient in mode
// whose query is reported every interval seconds; args mode selects dmap
// (cumulative) or dtail --query (non-cumulative).
func newReportTestClient(t *testing.T, mode MaprClientMode, argsMode omode.Mode,
	interval int) (*MaprClient, *tableCaptureLogger) {

	t.Helper()
	logger := &tableCaptureLogger{start: time.Now()}
	query := mustMaprClientQuery(t, fmt.Sprintf("from STATS select count(foo) group by foo interval %d", interval))
	c := &MaprClient{
		baseClient: baseClient{
			mu:   newBaseClientMu(),
			Args: config.Args{Mode: argsMode},
		},
		session: maprclient.NewSessionState(query, logging.NopLogger{}),
		mode:    mode,
	}
	c.loggers = LoggerDependencies{Client: logger}.normalized()
	c.runtime = newClientRuntimeBoundary(clientTestRuntimeConfig(), c.loggers)
	return c, logger
}

// sendReportTestAggregate merges one aggregate message of server through
// the client's receive path (Aggregate.Add and Merge).
func sendReportTestAggregate(t *testing.T, c *MaprClient, server string, count int) {
	t.Helper()
	aggregate := maprclient.NewAggregate(server, c.session, logging.NopLogger{})
	if err := aggregate.Add([]byte(fmt.Sprintf("a∥1∥count(foo)≔%d∥", count))); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := aggregate.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
}

// setReportTestServers gives the client one connection per server and
// returns the channels that end them (a closed one is a server whose
// session ended): the first report waits for the servers whose connection
// is up (liveServerNames).
func setReportTestServers(c *MaprClient, servers ...string) map[string]chan struct{} {
	ends := make(map[string]chan struct{}, len(servers))
	for _, server := range servers {
		ends[server] = make(chan struct{})
		c.connections = append(c.connections,
			&probeTestConnector{server: server, handler: doneOnlyHandler{done: ends[server]}})
	}
	return ends
}

// runReportTestReporter runs the reporter until the returned stop is called.
func runReportTestReporter(c *MaprClient) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.periodicReportResults(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// assertTable checks the time and count of the i-th printed table.
func assertTable(t *testing.T, logger *tableCaptureLogger, i int, wantAt time.Duration, wantCount float64) {
	t.Helper()
	at, values := logger.table(i)
	if at != wantAt || len(values) != 1 || values[0] != wantCount {
		t.Errorf("table %d at %v with %v, want at %v with count %v", i, at, values, wantAt, wantCount)
	}
}

// TestMaprReporterFirstTableFollowsFirstAggregates is the regression test of
// a query's first table appearing only 1.5 intervals after the query
// started: each dserver sends its first aggregates one interval after the
// start, and the reporter used to report at half an interval (nothing yet)
// and then once per interval. Now the first table is printed
// firstReportSettle after the latest server's first aggregates (srv2's,
// 0.5 s after srv1's), and the later ones once per interval from then on,
// for dmap (cumulative) and dtail --query (non-cumulative).
func TestMaprReporterFirstTableFollowsFirstAggregates(t *testing.T) {
	for name, argsMode := range map[string]omode.Mode{"dmap": omode.MapClient, "dtail": omode.TailClient} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, logger := newReportTestClient(t, DefaultMode, argsMode, 20)
				stop := runReportTestReporter(c)
				defer stop()

				time.Sleep(20 * time.Second) // the servers' first serialization
				sendReportTestAggregate(t, c, "srv1", 3)
				time.Sleep(500 * time.Millisecond)
				sendReportTestAggregate(t, c, "srv2", 4)
				time.Sleep(time.Second)
				synctest.Wait()
				if got := logger.tables(); len(got) != 1 {
					t.Fatalf("tables printed at %v, want one at 21.5s (1s after srv2's first aggregates)", got)
				}
				assertTable(t, logger, 0, 21500*time.Millisecond, 7)

				time.Sleep(18500 * time.Millisecond) // the second serialization
				sendReportTestAggregate(t, c, "srv1", 5)
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if got := logger.tables(); len(got) != 2 {
					t.Fatalf("tables printed at %v, want the second at 41.5s (one interval later)", got)
				}
				wantCount := 5.0 // dtail --query: what arrived since the last table
				if argsMode == omode.MapClient {
					wantCount = 12
				}
				assertTable(t, logger, 1, 41500*time.Millisecond, wantCount)
			})
		})
	}
}

// TestMaprReporterIntervalOneKeepsOneSendPerTable runs dtail --query
// (non-cumulative) with "interval 1": the servers send every second, with a
// little jitter, and every table must hold exactly one send of each server.
// A settle of a whole second would put every report on the instant of the
// next sends, mixing two of them in one table.
func TestMaprReporterIntervalOneKeepsOneSendPerTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 1)
		stop := runReportTestReporter(c)
		defer stop()

		const rounds = 5
		time.Sleep(time.Second)
		for round := 1; round <= rounds; round++ {
			time.Sleep(50 * time.Millisecond)
			sendReportTestAggregate(t, c, "srv1", round)
			time.Sleep(100 * time.Millisecond)
			sendReportTestAggregate(t, c, "srv2", round)
			time.Sleep(850 * time.Millisecond)
		}
		synctest.Wait()
		if got := logger.tables(); len(got) != rounds {
			t.Fatalf("tables printed at %v, want %d", got, rounds)
		}
		for i := range rounds {
			round := i + 1
			assertTable(t, logger, i, time.Duration(round)*time.Second+550*time.Millisecond, float64(2*round))
		}
	})
}

// TestMaprReporterTrailingServerJoinsNextTable checks the bound of the
// first report window: servers whose first aggregates keep trailing each
// other restart the settle, but the first table is not delayed past half an
// interval after the first server's; the rest are in the next table.
func TestMaprReporterTrailingServerJoinsNextTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, logger := newReportTestClient(t, DefaultMode, omode.MapClient, 4)
		stop := runReportTestReporter(c)
		defer stop()

		time.Sleep(4 * time.Second)
		sendReportTestAggregate(t, c, "srv1", 1)
		for _, server := range []string{"srv2", "srv3", "srv4"} {
			time.Sleep(900 * time.Millisecond) // each restarts the settle
			sendReportTestAggregate(t, c, server, 1)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := logger.tables(); len(got) != 2 {
			t.Fatalf("tables printed at %v, want two", got)
		}
		assertTable(t, logger, 0, 6*time.Second, 3)
		assertTable(t, logger, 1, 10*time.Second, 4)
	})
}

// TestMaprReporterPrintsNoTableWithoutAggregates checks that a query whose
// servers send nothing prints no (empty) table, and that a new generation
// starts over: its first table follows its own first aggregates.
func TestMaprReporterPrintsNoTableWithoutAggregates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, logger := newReportTestClient(t, DefaultMode, omode.MapClient, 20)
		stop := runReportTestReporter(c)
		defer stop()

		time.Sleep(65 * time.Second)
		synctest.Wait()
		if got := logger.tables(); len(got) != 0 {
			t.Fatalf("tables printed at %v without any aggregate", got)
		}

		if _, err := c.session.CommitQuery("from STATS select count(foo) group by foo interval 20", 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		sendReportTestAggregate(t, c, "srv1", 3)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := logger.tables(); len(got) != 1 {
			t.Fatalf("tables printed at %v, want one at 71s", got)
		}
		assertTable(t, logger, 0, 71*time.Second, 3)
	})
}

// TestMaprReporterCommitDuringSettle commits a new query while the first
// report of the previous one is settling: nothing of the replaced query is
// printed, and the new query's first table follows its own first
// aggregates.
func TestMaprReporterCommitDuringSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, logger := newReportTestClient(t, DefaultMode, omode.MapClient, 20)
		stop := runReportTestReporter(c)
		defer stop()

		time.Sleep(20 * time.Second)
		sendReportTestAggregate(t, c, "srv1", 3)
		time.Sleep(500 * time.Millisecond)
		if _, err := c.session.CommitQuery("from STATS select count(foo) group by foo interval 20", 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4500 * time.Millisecond)
		synctest.Wait()
		if got := logger.tables(); len(got) != 0 {
			t.Fatalf("tables printed at %v for the replaced query", got)
		}
		sendReportTestAggregate(t, c, "srv1", 4)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := logger.tables(); len(got) != 1 {
			t.Fatalf("tables printed at %v, want one at 26s", got)
		}
		assertTable(t, logger, 0, 26*time.Second, 4)
	})
}

// TestMaprReporterContinuousJobKeepsItsCadence checks that a continuous job
// (NonCumulativeMode) keeps reporting on its fixed cadence (half an interval,
// then every interval), so each of its outfile appends still covers one
// interval; it does not report early when aggregates arrive.
func TestMaprReporterContinuousJobKeepsItsCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, logger := newReportTestClient(t, NonCumulativeMode, omode.MapClient, 20)
		stop := runReportTestReporter(c)
		defer stop()

		time.Sleep(20 * time.Second)
		sendReportTestAggregate(t, c, "srv1", 3)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if got := logger.tables(); len(got) != 1 {
			t.Fatalf("tables printed at %v, want one at 30s", got)
		}
		assertTable(t, logger, 0, 30*time.Second, 3)
	})
}

// TestFirstReportWindowPostpone covers what the window does with a report
// that is due. An arrival that is ready together with the timer, which
// won, is taken as an arrival: the report is put off by the settle time
// (it used to run at once, which put every later report on the servers'
// send instants). A report before any arrival runs and leaves the window
// open; the settled one runs and ends it. While live servers are missing
// the report is put off wait by wait until they all arrived (e.g. their
// connections ended) or the window's end.
func TestFirstReportWindowPostpone(t *testing.T) {
	arrived := make(chan struct{}, 1)
	now := time.Now()
	never := func() bool { t.Fatal("liveness sampled without need"); return false }
	window := &firstReportWindow{arrived: arrived, wait: time.Second, limit: 5 * time.Second}
	if wait, ok := window.postpone(now, never); ok {
		t.Fatalf("a report without any arrival was put off by %v", wait)
	}
	window.reporting()
	if window.arrived == nil {
		t.Fatal("a report without any arrival ended the window")
	}

	arrived <- struct{}{}
	if wait, ok := window.postpone(now, func() bool { return true }); !ok || wait != time.Second {
		t.Fatalf("postpone with a pending arrival = %v, %v, want the settle time", wait, ok)
	}
	if wait, ok := window.postpone(now.Add(time.Second), never); ok {
		t.Fatalf("the settled report was put off by %v", wait)
	}
	window.reporting()
	if window.arrived != nil {
		t.Fatal("the settled report left the window open")
	}
	if _, ok := window.postpone(now.Add(2*time.Second), never); ok {
		t.Fatal("a report after the window ended was put off")
	}

	// One live server is missing: looked at again every wait.
	all := false
	live := func() bool { return all }
	window = &firstReportWindow{arrived: arrived, wait: time.Second, limit: 5 * time.Second}
	if got := window.settle(now, false); got != time.Second {
		t.Fatalf("settle with a missing server = %v, want a look after 1s", got)
	}
	if wait, ok := window.postpone(now.Add(time.Second), live); !ok || wait != time.Second {
		t.Fatalf("postpone with a missing server = %v, %v, want 1s", wait, ok)
	}
	if wait, ok := window.postpone(now.Add(4500*time.Millisecond), live); !ok || wait != 500*time.Millisecond {
		t.Fatalf("postpone near the window's end = %v, %v, want the rest of 500ms", wait, ok)
	}
	if _, ok := window.postpone(now.Add(5*time.Second), live); ok {
		t.Fatal("the report at the window's end was put off")
	}
	all = true // the missing server's connection ended
	if _, ok := window.postpone(now.Add(2*time.Second), live); ok {
		t.Fatal("the report was put off although every live server's aggregates arrived")
	}
}

// TestFirstReportWindowBounds checks the window's bounds and that
// continuous jobs get none.
func TestFirstReportWindowBounds(t *testing.T) {
	arrived := make(chan struct{}, 1)
	snapshot := maprclient.SessionSnapshot{AggregatesArrived: arrived}
	if disabled := (&MaprClient{mode: NonCumulativeMode}).newFirstReportWindow(snapshot, time.Second); disabled.arrived != nil {
		t.Fatal("continuous jobs got an early first report")
	}
	window := (&MaprClient{}).newFirstReportWindow(snapshot, time.Second)
	now := time.Now()
	if got := window.settle(now, true); got != 500*time.Millisecond {
		t.Fatalf("settle at interval 1 = %v, want 500ms", got)
	}
	if got := window.settle(now.Add(400*time.Millisecond), true); got != 100*time.Millisecond {
		t.Fatalf("settle near the window's end = %v, want 100ms", got)
	}
	if got := window.settle(now.Add(time.Second), true); got != 0 {
		t.Fatalf("settle past the window's end = %v, want 0", got)
	}
}

// TestNextReportDue: reports are planned one interval after the instant
// the previous one was planned for, whatever the report took; one that
// took longer than an interval skips the missed instants and keeps the
// phase.
func TestNextReportDue(t *testing.T) {
	due := time.Unix(1000, 0)
	const interval = 20 * time.Second
	tests := []struct {
		name     string
		took     time.Duration
		interval time.Duration
		want     time.Duration
	}{
		{"instant report", 0, interval, 20 * time.Second},
		{"slow report", 3 * time.Second, interval, 20 * time.Second},
		{"a whole interval", 20 * time.Second, interval, 40 * time.Second},
		{"longer than two intervals", 47 * time.Second, interval, 60 * time.Second},
		{"no interval", time.Second, 0, time.Second},
	}
	for _, tt := range tests {
		if got := nextReportDue(due, due.Add(tt.took), tt.interval).Sub(due); got != tt.want {
			t.Errorf("%s: next report %v after the planned instant, want %v", tt.name, got, tt.want)
		}
	}
}

// TestMaprReporterCadenceIgnoresReportTime: printing a table takes 300 ms
// here. The reports used to follow one interval after the end of the
// previous one, so every table came 300 ms later within its interval and
// the cadence drifted onto the servers' sends. Now each is planned one
// interval after the previous planned instant.
func TestMaprReporterCadenceIgnoresReportTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
		logger.printTime = 300 * time.Millisecond
		stop := runReportTestReporter(c)
		defer stop()

		time.Sleep(20 * time.Second)
		for round := 1; round <= 3; round++ {
			sendReportTestAggregate(t, c, "srv1", round)
			time.Sleep(20 * time.Second)
		}
		synctest.Wait()
		if got := logger.tables(); len(got) != 3 {
			t.Fatalf("tables printed at %v, want three", got)
		}
		for i, want := range []time.Duration{21 * time.Second, 41 * time.Second, 61 * time.Second} {
			assertTable(t, logger, i, want, float64(i+1))
		}
	})
}

// TestMaprReporterArrivalTogetherWithTheTimer: the servers' first
// aggregates arrive at the very instant a periodic report is due (30 s
// here). Whichever the reporter takes first, the later tables follow the
// settle time after the servers' sends (51 s), not at their instants. When
// the timer won against the signaled arrival, the reporter used to report
// at once and then every interval from there: at 50 s, the instant of the
// next send, whose table then came at 50 s or, when the report ran first,
// not before 70 s. The first table itself comes at 31 s, or at 30 s when
// the aggregates were merged after the due report looked for an arrival
// and before it rendered.
func TestMaprReporterArrivalTogetherWithTheTimer(t *testing.T) {
	for range 50 {
		synctest.Test(t, func(t *testing.T) {
			c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
			stop := runReportTestReporter(c)
			defer stop()

			time.Sleep(30 * time.Second)
			sendReportTestAggregate(t, c, "srv1", 1)
			time.Sleep(20 * time.Second)
			sendReportTestAggregate(t, c, "srv1", 2)
			time.Sleep(5 * time.Second)
			synctest.Wait()
			if got := logger.tables(); len(got) != 2 {
				t.Fatalf("tables printed at %v, want two", got)
			}
			if at, values := logger.table(0); (at != 30*time.Second && at != 31*time.Second) ||
				len(values) != 1 || values[0] != 1 {
				t.Errorf("table 0 at %v with %v, want at 30s or 31s with count 1", at, values)
			}
			assertTable(t, logger, 1, 51*time.Second, 2)
		})
	}
}

// TestMaprReporterFirstTableWaitsForEveryLiveServer is the regression test
// of the live run against four servers, two of whose first aggregates came
// 2.5 s after the other two's: the first table held only two servers and
// the next one two intervals of the late ones. Now the first table waits
// for every live server's first aggregates (here all four, so one table
// per interval holds one send of each), but no longer than half an
// interval after the first server's when one never sends.
func TestMaprReporterFirstTableWaitsForEveryLiveServer(t *testing.T) {
	t.Run("all four", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
			setReportTestServers(c, "pi0", "pi1", "pi2", "pi3")
			stop := runReportTestReporter(c)
			defer stop()

			// pi0/pi1 send at 20 s and 40 s, pi2/pi3 2.5 s after them.
			time.Sleep(20 * time.Second)
			// Each round sends other values: an unchanged table is not
			// printed again.
			for round := 1; round <= 2; round++ {
				sendReportTestAggregate(t, c, "pi0", round)
				sendReportTestAggregate(t, c, "pi1", round)
				time.Sleep(2500 * time.Millisecond)
				sendReportTestAggregate(t, c, "pi2", round)
				sendReportTestAggregate(t, c, "pi3", round)
				time.Sleep(17500 * time.Millisecond)
			}
			time.Sleep(5 * time.Second)
			synctest.Wait()
			if got := logger.tables(); len(got) != 2 {
				t.Fatalf("tables printed at %v, want two", got)
			}
			assertTable(t, logger, 0, 23500*time.Millisecond, 4)
			assertTable(t, logger, 1, 43500*time.Millisecond, 8)
		})
	})
	// pi0 sent and then its connection ended: pi1-pi3 are live, and pi3
	// is still missing, so the table waits for it (counting servers would
	// compare three arrivals with three live servers and report at once).
	t.Run("one sent and left, one missing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
			close(setReportTestServers(c, "pi0", "pi1", "pi2", "pi3")["pi0"])
			stop := runReportTestReporter(c)
			defer stop()

			time.Sleep(20 * time.Second)
			for _, server := range []string{"pi0", "pi1", "pi2"} {
				sendReportTestAggregate(t, c, server, 1)
			}
			time.Sleep(15 * time.Second)
			synctest.Wait()
			if got := logger.tables(); len(got) != 1 {
				t.Fatalf("tables printed at %v, want one", got)
			}
			assertTable(t, logger, 0, 30*time.Second, 3)
		})
	})
	// pi3 is live and missing when the others' aggregates arrive, but its
	// connection ends 3.2 s later: the table no longer waits for it until
	// the window's cap (30 s), only until the next look at the servers.
	t.Run("the missing one's connection ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
			ends := setReportTestServers(c, "pi0", "pi1", "pi2", "pi3")
			stop := runReportTestReporter(c)
			defer stop()

			time.Sleep(20 * time.Second)
			for _, server := range []string{"pi0", "pi1", "pi2"} {
				sendReportTestAggregate(t, c, server, 1)
			}
			time.Sleep(3200 * time.Millisecond)
			close(ends["pi3"])
			time.Sleep(10 * time.Second)
			synctest.Wait()
			if got := logger.tables(); len(got) != 1 {
				t.Fatalf("tables printed at %v, want one", got)
			}
			assertTable(t, logger, 0, 24*time.Second, 3)
		})
	})
	t.Run("one never sends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
			setReportTestServers(c, "pi0", "pi1", "pi2", "pi3")
			stop := runReportTestReporter(c)
			defer stop()

			time.Sleep(20 * time.Second)
			for _, server := range []string{"pi0", "pi1", "pi2"} {
				sendReportTestAggregate(t, c, server, 1)
			}
			time.Sleep(15 * time.Second)
			synctest.Wait()
			if got := logger.tables(); len(got) != 1 {
				t.Fatalf("tables printed at %v, want one", got)
			}
			assertTable(t, logger, 0, 30*time.Second, 3)
		})
	})
}

// TestAllLiveArrivedUsesTheConnections checks the real liveServerNames: the
// first report waits for the servers of the connections whose handler is
// not done, by name, and not for one whose session ended.
func TestAllLiveArrivedUsesTheConnections(t *testing.T) {
	c, _ := newReportTestClient(t, DefaultMode, omode.MapClient, 20)
	ended := make(chan struct{})
	close(ended)
	c.connections = []connectors.Connector{
		&probeTestConnector{server: "up", handler: doneOnlyHandler{done: make(chan struct{})}},
		&probeTestConnector{server: "gone", handler: doneOnlyHandler{done: ended}},
	}
	if got := c.liveServerNames(); len(got) != 1 || got[0] != "up" {
		t.Fatalf("liveServerNames() = %v, want [up]", got)
	}
	if c.allLiveArrived(c.session.Snapshot()) {
		t.Fatal("all live servers arrived before any aggregates")
	}
	sendReportTestAggregate(t, c, "gone", 1)
	if c.allLiveArrived(c.session.Snapshot()) {
		t.Fatal("an ended server's aggregates counted for the live one")
	}
	sendReportTestAggregate(t, c, "up", 1)
	if !c.allLiveArrived(c.session.Snapshot()) {
		t.Fatal("the live server's aggregates arrived, but not all live servers counted")
	}
}
