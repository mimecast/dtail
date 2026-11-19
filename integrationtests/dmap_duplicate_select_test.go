package integrationtests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDMapRejectsDuplicateSelect runs dmap with a query selecting count(lat)
// twice, serverless and with a dserver. Such a query used to count every line
// twice; dmap must now fail with a query error before reading anything and
// write no outfile.
func TestDMapRejectsDuplicateSelect(t *testing.T) {
	skipIfNotIntegrationTest(t)
	logger := NewTestLogger("TestDMapRejectsDuplicateSelect")
	defer writeLogFileIgnoringError(logger)

	dir := t.TempDir()
	input := filepath.Join(dir, "dup.log")
	if err := os.WriteFile(input, []byte(dmapAvgInput), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, withServer := range []bool{false, true} {
		mode := "serverless"
		if withServer {
			mode = "server"
		}
		t.Run(mode, func(t *testing.T) {
			outfile := filepath.Join(dir, mode+".csv")
			query := fmt.Sprintf("from STATS select status,count(lat),count(lat) "+
				"group by status outfile %s", outfile)
			stdout := filepath.Join(dir, mode+".stdout")
			exitCode, err := runDMapDuplicate(t, logger, stdout, input, query, withServer)
			if err == nil || exitCode == 0 {
				t.Fatalf("dmap with a duplicate select field succeeded (exit %d, %v)", exitCode, err)
			}
			want := "'select' clause has 'count(lat)' more than once"
			if got := readTestFile(t, stdout); !strings.Contains(got, want) {
				t.Fatalf("dmap output lacks %q:\n%s", want, got)
			}
			if _, statErr := os.Stat(outfile); !os.IsNotExist(statErr) {
				t.Fatalf("dmap wrote outfile %s (stat error %v)", outfile, statErr)
			}
		})
	}
}

// runDMapDuplicate runs dmap with query over input, serverless or through a
// fresh dserver, and returns its exit code and error.
func runDMapDuplicate(t *testing.T, logger *TestLogger, stdout, input, query string,
	withServer bool) (int, error) {

	t.Helper()
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
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	return runCommand(WithTestLogger(ctx, logger), t, stdout, "../dmap", args...)
}
