package clients

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// TestMaprClientCumulativeAppendWritesOnlyFinalResult runs a cumulative query
// with "outfile append" for several report intervals. Every interim report
// would append the whole result so far, so the outfile must get the final
// result exactly once, after what it held before, whichever client runs the
// query: dmap (CumulativeMode, DefaultMode with a dmap mode) or interactive
// dtail --query (DefaultMode in tail mode, cumulative by its outfile).
func TestMaprClientCumulativeAppendWritesOnlyFinalResult(t *testing.T) {
	tests := []struct {
		name     string
		mode     MaprClientMode
		argsMode omode.Mode
	}{
		{name: "cumulative mode", mode: CumulativeMode},
		{name: "dmap", mode: DefaultMode, argsMode: omode.MapClient},
		{name: "dtail query", mode: DefaultMode, argsMode: omode.TailClient},
	}
	for _, tt := range tests {
		for _, initial := range []string{"", "previous\n"} {
			t.Run(fmt.Sprintf("%s/initial=%q", tt.name, initial), func(t *testing.T) {
				outfile := filepath.Join(t.TempDir(), "result.csv")
				writeAppendTestFile(t, outfile, initial)
				want := initial + "7\n"
				if initial == "" {
					want = "count(foo)\n7\n"
				}
				runAppendTestQuery(t, outfile, tt.mode, tt.argsMode)
				assertScheduledTestFile(t, outfile, want)
			})
		}
	}
}

// runAppendTestQuery starts a client of mode running an "outfile append"
// query with a one second interval (first interim report after 0.5 s), whose
// connection receives one row and then stays connected for ten report
// intervals.
func runAppendTestQuery(t *testing.T, outfile string, mode MaprClientMode, argsMode omode.Mode) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		query := outfileTestQuery(t, outfile, true)
		query.Interval = time.Second
		c := newOutfileTestClient(query, mode)
		configureFinalTestConnection(c, func(context.Context) {
			mergeOutfileTestRow(t, c.session.Snapshot(), 7)
			time.Sleep(10 * time.Second)
		})
		c.Args = config.Args{Mode: argsMode}
		// Cancel after Start returned, so that the client's goroutines
		// waiting for the context end inside the bubble.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if status := c.Start(ctx, nil); status != 0 {
			t.Fatalf("Start status = %d", status)
		}
	})
}

// TestMaprClientCumulativeAppendInterimReports checks the report paths one by
// one: an interim report of a cumulative append query appends no rows and
// writes no .query file; it only creates a missing outfile empty (checking
// that it can be appended to), so the final report then appends the header
// and the rows once. A non-append cumulative query still writes interim
// results.
func TestMaprClientCumulativeAppendInterimReports(t *testing.T) {
	outfile := filepath.Join(t.TempDir(), "result.csv")
	query := outfileTestQuery(t, outfile, true)
	c := newOutfileTestClient(query, CumulativeMode)
	snapshot := c.session.Snapshot()
	mergeOutfileTestRow(t, snapshot, 7)

	for i := 0; i < 3; i++ {
		if err := c.writeResultsToOutfile(snapshot, false); err != nil {
			t.Fatalf("interim report %d error = %v", i, err)
		}
	}
	if got, err := os.ReadFile(outfile); err != nil || len(got) != 0 {
		t.Fatalf("outfile after interim reports = %q (error %v), want an empty file", got, err)
	}
	assertScheduledTestFile(t, outfile+".query", "")

	if err := c.writeResultsToOutfile(snapshot, true); err != nil {
		t.Fatalf("final report error = %v", err)
	}
	assertScheduledTestFile(t, outfile, "count(foo)\n7\n")
	assertScheduledTestFile(t, outfile+".query", query.RawQuery)

	overwrite := filepath.Join(t.TempDir(), "overwrite.csv")
	plain := newOutfileTestClient(outfileTestQuery(t, overwrite, false), CumulativeMode)
	plainSnapshot := plain.session.Snapshot()
	mergeOutfileTestRow(t, plainSnapshot, 7)
	if err := plain.writeResultsToOutfile(plainSnapshot, false); err != nil {
		t.Fatalf("interim report without append error = %v", err)
	}
	assertScheduledTestFile(t, overwrite, "count(foo)\n7\n")
}

// TestMaprClientCumulativeAppendInterimReportsUnwritableOutfile checks that an
// interim report of a cumulative append query fails when the outfile can not
// be opened for appending (a missing directory, a directory at its path), so
// the reporter logs it every interval, not only the final write at the end.
func TestMaprClientCumulativeAppendInterimReportsUnwritableOutfile(t *testing.T) {
	dir := t.TempDir()
	for name, outfile := range map[string]string{
		"missing directory": filepath.Join(dir, "missing", "result.csv"),
		"directory":         dir,
	} {
		t.Run(name, func(t *testing.T) {
			c := newOutfileTestClient(outfileTestQuery(t, outfile, true), CumulativeMode)
			snapshot := c.session.Snapshot()
			mergeOutfileTestRow(t, snapshot, 7)
			if err := c.writeResultsToOutfile(snapshot, false); err == nil {
				t.Fatal("interim report to an unwritable outfile succeeded")
			}
			if name == "missing directory" {
				assertScheduledTestFile(t, filepath.Dir(outfile), "")
			}
		})
	}
}

// TestMaprClientDropQueryAppendsOnce checks that interactive dtail leaving a
// cumulative append query (:tail, dropQuery) appends its result once, and that
// nothing is appended again when the client ends afterwards.
func TestMaprClientDropQueryAppendsOnce(t *testing.T) {
	outfile := filepath.Join(t.TempDir(), "result.csv")
	c := newOutfileTestClient(outfileTestQuery(t, outfile, true), DefaultMode)
	c.mu = newBaseClientMu()
	c.Args = config.Args{Mode: omode.TailClient}
	mergeOutfileTestRow(t, c.session.Snapshot(), 7)

	c.dropQuery(2)
	assertScheduledTestFile(t, outfile, "count(foo)\n7\n")
	if err := c.reportResults(true); err != nil {
		t.Fatalf("reportResults(true) after dropQuery error = %v", err)
	}
	assertScheduledTestFile(t, outfile, "count(foo)\n7\n")
}

// TestMaprClientAppendReplacedResultSkipsOtherQueries checks that a reload
// replacing a query writes nothing for a non-append cumulative query (its
// interim reports wrote its result) nor for a non-cumulative append query
// (continuous jobs, each interval appended its rows).
func TestMaprClientAppendReplacedResultSkipsOtherQueries(t *testing.T) {
	tests := []struct {
		name       string
		mode       MaprClientMode
		appendMode bool
	}{
		{name: "cumulative without append", mode: CumulativeMode},
		{name: "non-cumulative append", mode: NonCumulativeMode, appendMode: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outfile := filepath.Join(t.TempDir(), "result.csv")
			c := newOutfileTestClient(outfileTestQuery(t, outfile, tt.appendMode), tt.mode)
			mergeOutfileTestRow(t, c.session.Snapshot(), 7)
			c.appendReplacedResult(c.session.Snapshot())
			assertScheduledTestFile(t, outfile, "")
		})
	}
}

// TestMaprClientReloadAppendsReplacedQueryResult checks that an interactive
// reload replacing a cumulative "outfile append" query appends what that
// query received once, as no interim report wrote it, and that a reload that
// fails to commit appends nothing (the query keeps running and appends at
// its end).
func TestMaprClientReloadAppendsReplacedQueryResult(t *testing.T) {
	outfile := filepath.Join(t.TempDir(), "result.csv")
	query := outfileTestQuery(t, outfile, true)
	c := newOutfileTestClient(query, DefaultMode)
	c.mu = newBaseClientMu()
	c.Args = config.Args{Mode: omode.MapClient}
	mergeOutfileTestRow(t, c.session.Snapshot(), 7)

	if err := c.commitSessionSpec(SessionSpec{Query: "from STATS select"}, 2); err == nil {
		t.Fatal("commitSessionSpec() with an invalid query succeeded")
	}
	assertScheduledTestFile(t, outfile, "")
	if got := c.session.Snapshot().Query; got != query {
		t.Fatalf("failed commit replaced the query with %v", got)
	}

	if err := c.commitSessionSpec(SessionSpec{Query: query.RawQuery}, 3); err != nil {
		t.Fatalf("commitSessionSpec() error = %v", err)
	}
	assertScheduledTestFile(t, outfile, "count(foo)\n7\n")

	mergeOutfileTestRow(t, c.session.Snapshot(), 3)
	if err := c.reportResults(true); err != nil {
		t.Fatalf("reportResults(true) error = %v", err)
	}
	assertScheduledTestFile(t, outfile, "count(foo)\n7\n3\n")
}

func writeAppendTestFile(t *testing.T, path, content string) {
	t.Helper()
	if content == "" {
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
