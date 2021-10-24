package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDMap(t *testing.T) {
	if !config.Env("DTAIL_RUN_INTEGRATION_TESTS") {
		t.Log("Skipping")
		return
	}
	inFile := "mapr_testdata.log"
	csvFile := "dmap.csv.tmp"
	expectedCsvFile := "dmap.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($line),last($time),"+
		"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) "+
		"group by $hostname outfile %s", csvFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t, "../dmap",
		"--cfg", "none",
		"--query", query,
		"--logger", "stdout",
		"--logLevel", "error",
		"--noColor",
		inFile)

	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFiles(t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(csvFile)
	os.Remove(queryFile)
}

func TestDMap2(t *testing.T) {
	if !config.Env("DTAIL_RUN_INTEGRATION_TESTS") {
		t.Log("Skipping")
		return
	}
	inFile := "mapr_testdata.log"
	stdoutFile := "dmap2.stdout.tmp"
	csvFile := "dmap2.csv.tmp"
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap2.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	_, err := runCommand(context.TODO(), t, stdoutFile,
		"../dmap", "--query", query, "--cfg", "none", inFile)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContents(t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
	os.Remove(csvFile)
	os.Remove(queryFile)
}

func TestDMap3(t *testing.T) {
	if !config.Env("DTAIL_RUN_INTEGRATION_TESTS") {
		t.Log("Skipping")
		return
	}
	inFile := "mapr_testdata.log"
	stdoutFile := "dmap3.stdout.tmp"
	csvFile := "dmap3.csv.tmp"
	expectedCsvFile := "dmap3.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap3.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t, "../dmap",
		"--query", query,
		"--cfg", "none",
		"--logger", "stdout",
		"--logLevel", "info",
		"--noColor",
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile,
		inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile, inFile)

	if err != nil {
		t.Error(err)
		return
	}
	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFilesContents(t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, queryFile, expectedQueryFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(stdoutFile)
	os.Remove(csvFile)
	os.Remove(queryFile)
}
