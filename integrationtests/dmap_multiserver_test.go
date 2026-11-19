package integrationtests

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDMapMultiServer tests MapReduce operations across multiple servers
// Note: Due to DTAIL_INTEGRATION_TEST_RUN_MODE, all servers report hostname as "integrationtest"
func TestDMapMultiServer(t *testing.T) {
	skipIfNotIntegrationTest(t)

	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMapMultiServer")
	defer testLogger.WriteLogFile()

	// Start three servers
	server1 := NewTestServer(t)
	server2 := NewTestServer(t)
	server3 := NewTestServer(t)

	if err := server1.Start("error"); err != nil {
		t.Fatal(err)
	}
	if err := server2.Start("error"); err != nil {
		t.Fatal(err)
	}
	if err := server3.Start("error"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1 * time.Second)

	// Test GROUP BY with multiple servers
	csvFile := "dmap_multi_groupby.csv.tmp"
	outFile := "dmap_multi_groupby.stdout.tmp"
	cleanupFiles(t, csvFile, outFile)

	paths := GetStandardTestPaths()
	// Group by time to show aggregation across servers
	query := fmt.Sprintf("from STATS select $time,count($line),avg($goroutines) "+
		"group by $time order by count($line) desc limit 10 "+
		"outfile %s", csvFile)

	args := NewCommandArgs()
	args.Servers = []string{server1.Address(), server2.Address(), server3.Address()}
	args.TrustAllHosts = true
	args.NoColor = true
	args.Files = []string{paths.MaprTestData}
	args.ExtraArgs = []string{"--query", query}

	ctx, cancel := createTestContextWithTimeout(t)
	ctx = WithTestLogger(ctx, testLogger)
	defer cancel()

	_, err := runCommand(ctx, t, outFile,
		"../dmap", args.ToSlice()...)
	if err != nil {
		t.Fatal(err)
	}

	// Check results
	csvContent, err := os.ReadFile(csvFile)
	if err != nil {
		t.Fatal(err)
	}

	csvStr := string(csvContent)
	t.Logf("GROUP BY time CSV (top 10):\n%s", csvStr)

	// Verify we got results
	lines := strings.Split(strings.TrimSpace(csvStr), "\n")
	if len(lines) < 2 {
		t.Fatal("Expected at least 2 lines in CSV")
	}

	// Verify header
	if !strings.Contains(lines[0], "$time,count($line),avg($goroutines)") {
		t.Errorf("Unexpected header: %s", lines[0])
	}

	// The most common timestamps should have high counts (multiples of 3 since we have 3 servers)
	dataLine := lines[1] // First data line (highest count)
	fields := strings.Split(dataLine, ",")
	if len(fields) < 2 {
		t.Fatalf("Expected at least 2 fields in data line, got %d", len(fields))
	}

	count, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("Failed to parse count: %v", err)
	}

	// The top counts should be relatively high (multiple occurrences across servers)
	if count < 20 {
		t.Errorf("Expected higher count for top result, got %d", count)
	}

	t.Logf("Successfully aggregated data from %d servers", 3)
	t.Logf("Top timestamp '%s' appeared %d times across all servers", fields[0], count)

	// Log file verification
	testLogger.LogFileComparison(csvFile, "GROUP BY results", "content verification")
}