package integrationtests

import (
	"fmt"
	"os"
	"testing"
)

func TestDMap(t *testing.T) {
	inFile := "mapr_testdata.log"
	stdoutFile := "dmap.stdout.tmp"
	csvFile := "dmap.csv.tmp"
	expectedCsvFile := "dmap.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($line),last($time),avg($goroutines),min(concurrentConnections),max(lifetimeConnections) group by $hostname outfile %s", csvFile)

	if _, err := runCommand(t, "../dmap", []string{"-query", query, inFile}, stdoutFile); err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, csvFile, expectedCsvFile); err != nil {
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

func TestDMap2(t *testing.T) {
	inFile := "mapr_testdata.log"
	stdoutFile := "dmap2.stdout.tmp"
	csvFile := "dmap2.csv.tmp"
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	expectedQueryFile := "dmap2.csv.query.expected"

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),avg($goroutines),min($goroutines) group by $time order by count($time) outfile %s", csvFile)

	if _, err := runCommand(t, "../dmap", []string{"-query", query, inFile}, stdoutFile); err != nil {
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
