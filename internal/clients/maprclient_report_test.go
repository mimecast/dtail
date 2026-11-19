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
	start  time.Time
	mu     sync.Mutex
	at     []time.Duration
	values [][]float64
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
	defer l.mu.Unlock()
	l.at = append(l.at, time.Since(l.start))
	l.values = append(l.values, values)
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

// TestFirstReportWindowEndsOnReportWithPendingArrival covers the timer and an
// arrival being ready together: the report that runs first already holds
// the arrived aggregates, so the window ends and no settled report follows.
// A report before any arrival leaves the window open. It also checks the
// window's bounds and that continuous jobs get none.
func TestFirstReportWindowEndsOnReportWithPendingArrival(t *testing.T) {
	arrived := make(chan struct{}, 1)
	window := &firstReportWindow{arrived: arrived, wait: time.Second, limit: 2 * time.Second}
	window.reporting()
	if window.arrived == nil {
		t.Fatal("a report without any arrival ended the window")
	}
	arrived <- struct{}{}
	window.reporting()
	if window.arrived != nil {
		t.Fatal("a report with a pending arrival left the window open")
	}

	snapshot := maprclient.SessionSnapshot{AggregatesArrived: arrived}
	if disabled := (&MaprClient{mode: NonCumulativeMode}).newFirstReportWindow(snapshot, time.Second); disabled.arrived != nil {
		t.Fatal("continuous jobs got an early first report")
	}
	window = (&MaprClient{}).newFirstReportWindow(snapshot, time.Second)
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
			c.liveServers = func() []string { return []string{"pi0", "pi1", "pi2", "pi3"} }
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
			c.liveServers = func() []string { return []string{"pi1", "pi2", "pi3"} }
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
	t.Run("one never sends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, logger := newReportTestClient(t, DefaultMode, omode.TailClient, 20)
			c.liveServers = func() []string { return []string{"pi0", "pi1", "pi2", "pi3"} }
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
