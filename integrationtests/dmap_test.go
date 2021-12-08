package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDMap1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	testTable := map[string]string{
		"a": "from STATS select count($line),last($time)," +
			"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
			"group by $hostname",
		"b": "from STATS select count($line),last($time)," +
			"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
			"group by $hostname where lifetimeConnections >= 3",
		"c": "from STATS select count($line),last($time)," +
			"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
			"group by $hostname where $time eq \"20211002-071949\"",
	}

	for subtestName, query := range testTable {
		t.Log("Testing dmap with input file")
		if err := testDmap1(t, query, subtestName, false); err != nil {
			t.Error(err)
			return
		}
		t.Log("Testing dmap with stdin input pipe")
		if err := testDmap1(t, query, subtestName, true); err != nil {
			t.Error(err)
			return
		}
	}
}

func testDmap1(t *testing.T, query, subtestName string, usePipe bool) error {
	inFile := "mapr_testdata.log"
	csvFile := fmt.Sprintf("dmap1%s.csv.tmp", subtestName)
	expectedCsvFile := fmt.Sprintf("dmap1%s.csv.expected", subtestName)
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := fmt.Sprintf("dmap1%s.csv.query.expected", subtestName)
	query = fmt.Sprintf("%s outfile %s", query, csvFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdoutCh, stderrCh <-chan string
	var cmdErrCh <-chan error
	var err error

	if usePipe {
		stdoutCh, stderrCh, cmdErrCh, err = startCommand(ctx, t,
			inFile, "../dmap",
			"--cfg", "none",
			"--query", query,
			"--logger", "stdout",
			"--logLevel", "error",
			"--noColor")
	} else {
		stdoutCh, stderrCh, cmdErrCh, err = startCommand(ctx, t,
			"", "../dmap",
			"--cfg", "none",
			"--query", query,
			"--logger", "stdout",
			"--logLevel", "error",
			"--noColor",
			inFile)
	}

	if err != nil {
		return err
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFiles(t, csvFile, expectedCsvFile); err != nil {
		return err
	}
	if err := compareFiles(t, queryFile, expectedQueryFile); err != nil {
		return err
	}

	os.Remove(csvFile)
	os.Remove(queryFile)
	return nil
}

func TestDMap2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	inFile := "mapr_testdata.log"
	outFile := "dmap2.stdout.tmp"
	csvFile := "dmap2.csv.tmp"
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap2.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	_, err := runCommand(context.TODO(), t, outFile,
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

	os.Remove(outFile)
	os.Remove(csvFile)
	os.Remove(queryFile)
}

func TestDMap3(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	inFile := "mapr_testdata.log"
	outFile := "dmap3.stdout.tmp"
	csvFile := "dmap3.csv.tmp"
	expectedCsvFile := "dmap3.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap3.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dmap",
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

	os.Remove(outFile)
	os.Remove(csvFile)
	os.Remove(queryFile)
}
