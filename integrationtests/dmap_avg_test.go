package integrationtests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dmapAvgInput has lines with a numeric lat, without lat and with a lat that
// is no number. Every line has a status, so every line is a sample of its
// group, but only the numeric lat values may be averaged.
const dmapAvgInput = `INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|lat=10|status=ok
INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|status=ok
INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|lat=abc|status=ok
INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|lat=20|status=ok
INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|status=fail
`

// TestDMapAvgSparseValues runs avg() over lines lacking the averaged field or
// carrying a value that is no number, serverless (client-side aggregation)
// and through a dserver (server aggregation, avg() value count on the wire,
// client merge), and checks that both modes write the same outfile with the
// average of the numeric values only (formerly it divided by every line).
func TestDMapAvgSparseValues(t *testing.T) {
	skipIfNotIntegrationTest(t)
	logger := NewTestLogger("TestDMapAvgSparseValues")
	defer writeLogFileIgnoringError(logger)

	dir := t.TempDir()
	input := filepath.Join(dir, "avg.log")
	if err := os.WriteFile(input, []byte(dmapAvgInput), 0o644); err != nil {
		t.Fatal(err)
	}
	serverless := runDMapAvg(t, logger, dir, input, false)
	server := runDMapAvg(t, logger, dir, input, true)
	if serverless != server {
		t.Fatalf("server mode outfile differs from serverless:\nserverless:\n%s\nserver:\n%s",
			serverless, server)
	}
	// Group ok: avg of 10 and 20; group fail has no lat value at all.
	want := "status,avg(lat),sum(lat),count(status)\n" +
		"fail,0.000000,0.000000,1\n" +
		"ok,15.000000,30.000000,4\n"
	if serverless != want {
		t.Fatalf("outfile =\n%s\nwant\n%s", serverless, want)
	}
}

// runDMapAvg runs the avg() query of TestDMapAvgSparseValues serverless or
// through a fresh dserver and returns the written outfile.
func runDMapAvg(t *testing.T, logger *TestLogger, dir, input string, withServer bool) string {
	t.Helper()
	mode := "serverless"
	if withServer {
		mode = "server"
	}
	outfile := filepath.Join(dir, mode+".csv")
	query := fmt.Sprintf("from STATS select status,avg(lat),sum(lat),count(status) "+
		"group by status outfile %s", outfile)
	args := []string{"--query", query, "--cfg", "none", input}
	parent := context.Background()
	if withServer {
		server := NewTestServer(t)
		if err := server.Start("error"); err != nil {
			t.Fatal(err)
		}
		parent = server.ctx
		commandArgs := NewCommandArgs()
		commandArgs.Servers = []string{server.Address()}
		commandArgs.TrustAllHosts = true
		commandArgs.NoColor = true
		commandArgs.Files = []string{input}
		commandArgs.ExtraArgs = []string{"--query", query}
		args = commandArgs.ToSlice()
	}
	// Generous: a loaded host (e.g. the race detector) may be slow.
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	ctx = WithTestLogger(ctx, logger)
	stdout := filepath.Join(dir, mode+".stdout")
	if _, err := runCommand(ctx, t, stdout, "../dmap", args...); err != nil {
		t.Fatalf("%s dmap: %v", mode, err)
	}
	return readTestFile(t, outfile)
}
