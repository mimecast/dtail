package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDMapLargeFile(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMapLargeFile")
	defer testLogger.WriteLogFile()
	
	// Generate the large test file once for both test modes
	largeFile := "dmap_large_100mb.log.tmp"
	t.Log("Generating 100MB test file...")
	generateLargeMapReduceFile(t, largeFile)
	
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMapLargeFile",
		ServerlessTest: func(t *testing.T) { testDMapLargeFileServerless(t, testLogger, largeFile) },
		ServerTest:     func(t *testing.T) { testDMapLargeFileWithServer(t, testLogger, largeFile) },
	})
}

// generateLargeMapReduceFile generates a 100MB log file with MapReduce data
func generateLargeMapReduceFile(t *testing.T, filename string) {
	t.Helper()
	
	// Clean up before test
	if _, err := os.Stat(filename); err == nil {
		if err := os.Remove(filename); err != nil {
			t.Fatalf("Failed to remove existing file %s: %v", filename, err)
		}
	}
	
	file, err := os.Create(filename)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer file.Close()
	
	// Generate data until we reach approximately 100MB
	targetSize := int64(100 * 1024 * 1024) // 100MB
	currentSize := int64(0)
	lineNum := 0
	
	// Pre-generate some hostnames for variety
	hostnames := []string{"server01", "server02", "server03", "server04", "server05", 
		"server06", "server07", "server08", "server09", "server10"}
	
	startTime := time.Now()
	for currentSize < targetSize {
		lineNum++
		
		// Generate varied data for more realistic testing
		hostname := hostnames[lineNum%len(hostnames)]
		timestamp := fmt.Sprintf("%02d%02d-%02d%02d%02d", 
			10+(lineNum/86400)%12, (lineNum/3600)%30+1, 
			(lineNum/3600)%24, (lineNum/60)%60, lineNum%60)
		goroutines := 10 + (lineNum % 50)
		cgocalls := lineNum % 100
		cpus := 1 + (lineNum % 8)
		loadavg := float64(lineNum%100) / 100.0
		uptime := fmt.Sprintf("%dh%dm%ds", lineNum/3600, (lineNum/60)%60, lineNum%60)
		currentConnections := lineNum % 20
		lifetimeConnections := 1000 + lineNum
		
		// DTail format: INFO|date-time|pid|caller|cpus|goroutines|cgocalls|loadavg|uptime|MAPREDUCE:STATS|key=value|...
		line := fmt.Sprintf("INFO|%s|1|stats.go:56|%d|%d|%d|%.2f|%s|MAPREDUCE:STATS|hostname=%s|currentConnections=%d|lifetimeConnections=%d\n",
			timestamp, cpus, goroutines, cgocalls, loadavg, uptime, hostname, currentConnections, lifetimeConnections)
		
		n, err := file.WriteString(line)
		if err != nil {
			t.Fatalf("Failed to write to test file: %v", err)
		}
		currentSize += int64(n)
	}
	
	elapsed := time.Since(startTime)
	t.Logf("Generated %d lines (%d MB) in %v", lineNum, currentSize/(1024*1024), elapsed)
}

func testDMapLargeFileServerless(t *testing.T, logger *TestLogger, largeFile string) {
	csvFile := "dmap_large_serverless.csv.tmp"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	outFile := "dmap_large_serverless.stdout.tmp"
	
	// Clean up output files before test (but not the large input file)
	cleanupFiles(t, csvFile, queryFile, outFile)
	
	// Run several queries on the large file
	queries := []struct {
		name  string
		query string
	}{
		{
			name: "CountByHostname",
			query: fmt.Sprintf("from STATS select count($line),sum(lifetimeConnections),avg(goroutines),min(currentConnections),max(lifetimeConnections) "+
				"group by hostname order by count($line) desc outfile %s", csvFile),
		},
		{
			name: "HighConnectionsFilter",
			query: fmt.Sprintf("from STATS select hostname,count($line),avg(currentConnections),max(currentConnections) "+
				"group by hostname where currentConnections > 10 order by max(currentConnections) desc outfile %s", csvFile),
		},
		{
			name: "LoadDistribution",
			query: fmt.Sprintf("from STATS select hostname,count($line),avg(cpus),avg($loadavg),max($loadavg) "+
				"group by hostname order by avg($loadavg) desc outfile %s", csvFile),
		},
	}
	
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			// Clean up output files for each query
			cleanupFiles(t, csvFile, queryFile, outFile)
			
			ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			ctx := WithTestLogger(ctxTimeout, logger)
			defer cancel()
			
			startTime := time.Now()
			_, err := runCommand(ctx, t, outFile,
				"../dmap", "--query", tc.query, "--cfg", "none", largeFile)
			elapsed := time.Since(startTime)
			
			if err != nil {
				t.Errorf("Query failed: %v", err)
				return
			}
			
			t.Logf("Query completed in %v", elapsed)
			
			// Verify the output file was created
			if _, err := os.Stat(csvFile); os.IsNotExist(err) {
				t.Error("Expected CSV output file was not created")
			} else {
				// Log file size for verification
				if info, err := os.Stat(csvFile); err == nil {
					t.Logf("Output CSV size: %d bytes", info.Size())
				}
			}
			
			// Verify query file
			if err := verifyQueryFile(t, queryFile, tc.query); err != nil {
				t.Error(err)
			}
		})
	}
}

func testDMapLargeFileWithServer(t *testing.T, logger *TestLogger, largeFile string) {
	csvFile := "dmap_large_server.csv.tmp"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	outFile := "dmap_large_server.stdout.tmp"
	
	// Clean up output files before test (but not the large input file)
	cleanupFiles(t, csvFile, queryFile, outFile)
	
	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}
	
	// Run several queries on the large file
	queries := []struct {
		name  string
		query string
	}{
		{
			name: "CountByHostname",
			query: fmt.Sprintf("from STATS select count($line),sum(lifetimeConnections),avg(goroutines),min(currentConnections),max(lifetimeConnections) "+
				"group by hostname order by count($line) desc outfile %s", csvFile),
		},
		{
			name: "HighConnectionsFilter", 
			query: fmt.Sprintf("from STATS select hostname,count($line),avg(currentConnections),max(currentConnections) "+
				"group by hostname where currentConnections > 10 order by max(currentConnections) desc outfile %s", csvFile),
		},
		{
			name: "LoadDistribution",
			query: fmt.Sprintf("from STATS select hostname,count($line),avg(cpus),avg($loadavg),max($loadavg) "+
				"group by hostname order by avg($loadavg) desc outfile %s", csvFile),
		},
	}
	
	baseArgs := NewCommandArgs()
	baseArgs.Servers = []string{server.Address()}
	baseArgs.TrustAllHosts = true
	baseArgs.NoColor = true
	baseArgs.Files = []string{largeFile}
	
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			// Clean up output files for each query
			cleanupFiles(t, csvFile, queryFile, outFile)
			
			args := *baseArgs
			args.ExtraArgs = []string{"--query", tc.query}
			
			startTime := time.Now()
			_, err := runCommand(server.ctx, t, outFile,
				"../dmap", args.ToSlice()...)
			elapsed := time.Since(startTime)
			
			if err != nil {
				t.Errorf("Query failed: %v", err)
				return
			}
			
			t.Logf("Query completed in %v", elapsed)
			
			// Verify the output file was created
			if _, err := os.Stat(csvFile); os.IsNotExist(err) {
				t.Error("Expected CSV output file was not created")
			} else {
				// Log file size for verification
				if info, err := os.Stat(csvFile); err == nil {
					t.Logf("Output CSV size: %d bytes", info.Size())
				}
			}
			
			// Verify query file
			if err := verifyQueryFile(t, queryFile, tc.query); err != nil {
				t.Error(err)
			}
		})
	}
	
	// Note: CSV verification should be done per query, not globally
	// since csvFile gets overwritten by each query
}

// verifyLargeFileResults does basic sanity checks on the output
func verifyLargeFileResults(ctx context.Context, t *testing.T, csvFile string) error {
	t.Helper()
	
	data, err := os.ReadFile(csvFile)
	if err != nil {
		return fmt.Errorf("failed to read CSV file: %w", err)
	}
	
	lines := string(data)
	if len(lines) == 0 {
		return fmt.Errorf("CSV file is empty")
	}
	
	// Basic check: should have at least a header and some data
	lineCount := 0
	for _, line := range lines {
		if line == '\n' {
			lineCount++
		}
	}
	
	if lineCount < 2 {
		return fmt.Errorf("CSV file has insufficient data (only %d lines)", lineCount)
	}
	
	t.Logf("CSV output has %d lines", lineCount)
	return nil
}