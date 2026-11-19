package integrationtests

import (
	"context"
	"fmt"
	"testing"
)

// TestDMapCSVMultiFile regression-tests the bug where the CSV log-format
// parser consumed the first line of every file as a header, silently
// treating the second (and later) files' header rows as data rows and
// corrupting aggregates. With two CSV files that have 2 and 3 data rows
// respectively we must observe exactly 5 data rows — not 6.
func TestDMapCSVMultiFile(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMapCSVMultiFile")
	defer testLogger.WriteLogFile()
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMapCSVMultiFile",
		ServerlessTest: func(t *testing.T) { testDMapCSVMultiFileServerless(t, testLogger) },
		ServerTest:     func(t *testing.T) { testDMapCSVMultiFileWithServer(t, testLogger) },
	})
}

func testDMapCSVMultiFileServerless(t *testing.T, logger *TestLogger) {
	inFileA := "dmap_csv_multifile_a.csv.in"
	inFileB := "dmap_csv_multifile_b.csv.in"
	csvFile := "dmap_csv_multifile_serverless.csv.tmp"
	expectedCsvFile := "dmap_csv_multifile.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	outFile := "dmap_csv_multifile_serverless.stdout.tmp"
	cleanupFiles(t, csvFile, queryFile, outFile)

	query := fmt.Sprintf("select count($line) group by * logformat csv outfile %s", csvFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	ctx := WithTestLogger(ctxTimeout, logger)
	defer cancel()
	_, err := runCommand(ctx, t, outFile,
		"../dmap", "--query", query, "--cfg", "none", inFileA, inFileB)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDMapCSVMultiFileWithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	inFileA := "dmap_csv_multifile_a.csv.in"
	inFileB := "dmap_csv_multifile_b.csv.in"
	csvFile := "dmap_csv_multifile_server.csv.tmp"
	expectedCsvFile := "dmap_csv_multifile.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	outFile := "dmap_csv_multifile_server.stdout.tmp"
	cleanupFiles(t, csvFile, queryFile, outFile)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}

	query := fmt.Sprintf("select count($line) group by * logformat csv outfile %s", csvFile)

	args := NewCommandArgs()
	args.Servers = []string{server.Address()}
	args.TrustAllHosts = true
	args.NoColor = true
	args.Files = []string{inFileA, inFileB}
	args.ExtraArgs = []string{"--query", query}

	_, err := runCommand(server.ctx, t, outFile,
		"../dmap", args.ToSlice()...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}
