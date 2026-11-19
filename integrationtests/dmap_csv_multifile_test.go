package integrationtests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	defer writeLogFileIgnoringError(testLogger)
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

// TestDMapCSVSameBaseName regression-tests CSV files with the same base name
// in different directories. Their reads share a glob ID, under which the CSV
// parser used to keep one header for both: the second file's header row was
// mapped as data, and with a different column order every row of it was
// mapped against the first file's header.
func TestDMapCSVSameBaseName(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMapCSVSameBaseName")
	defer writeLogFileIgnoringError(testLogger)
	runDualModeTest(t, DualModeTest{
		Name: "TestDMapCSVSameBaseName",
		ServerlessTest: func(t *testing.T) {
			testDMapCSVSameBaseName(t, testLogger, false)
		},
		ServerTest: func(t *testing.T) {
			testDMapCSVSameBaseName(t, testLogger, true)
		},
	})
}

func testDMapCSVSameBaseName(t *testing.T, logger *TestLogger, withServer bool) {
	dir := t.TempDir()
	inFileA := writeSameBaseNameCSV(t, dir, "a", "label,value\nx,1\ny,2\n")
	inFileB := writeSameBaseNameCSV(t, dir, "b", "value,label\n3,x\n4,z\n")
	csvFile := filepath.Join(dir, "out.csv")
	outFile := filepath.Join(dir, "stdout.txt")
	query := fmt.Sprintf("select count($line),label group by label logformat csv outfile %s", csvFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	defer cancel()
	ctx := WithTestLogger(ctxTimeout, logger)
	args := []string{"--query", query, "--cfg", "none", inFileA, inFileB}
	if withServer {
		server := NewTestServer(t)
		if err := server.Start("error"); err != nil {
			t.Fatal(err)
		}
		ctx = WithTestLogger(server.ctx, logger)
		commandArgs := NewCommandArgs()
		commandArgs.Servers = []string{server.Address()}
		commandArgs.TrustAllHosts = true
		commandArgs.NoColor = true
		commandArgs.Files = []string{inFileA, inFileB}
		commandArgs.ExtraArgs = []string{"--query", query}
		args = commandArgs.ToSlice()
	}
	if _, err := runCommand(ctx, t, outFile, "../dmap", args...); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(readTestFile(t, csvFile)), "\n")
	slices.Sort(lines)
	want := []string{"1,y", "1,z", "2,x", "count($line),label"}
	if !slices.Equal(lines, want) {
		t.Errorf("dmap output = %q, want %q", lines, want)
	}
}

func writeSameBaseNameCSV(t *testing.T, dir, subdir, content string) string {
	t.Helper()
	path := filepath.Join(dir, subdir, "same.csv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
