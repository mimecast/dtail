package integrationtests

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// slowClientGroups is the number of groups of TestDMapSlowClientGetsEveryGroup.
	// Their final result (several MiB) is far more than the SSH channel
	// window and the session's queues hold, so dserver's final serialization
	// blocks while the client is stopped.
	slowClientGroups = 100000
	// slowClientLinesPerGroup is how many input lines each group has.
	slowClientLinesPerGroup = 2
	// slowClientPause is how long the client is stopped: longer than the
	// fixed 10 s deadline dserver's final serialization used to have, after
	// which it dropped the unsent groups and still closed the session
	// normally (an incomplete outfile with exit status 0).
	slowClientPause = 15 * time.Second
)

// TestDMapSlowClientGetsEveryGroup runs a server-mode dmap with many groups
// and stops (SIGSTOP) the client while dserver sends the final result, for
// longer than the former 10 s deadline of that final serialization. dserver
// must wait for the client instead of dropping groups: the outfile has every
// group with its full count, and the server log has neither an interrupted
// serialization nor an incomplete final result. Should the result
// nevertheless not reach the client in full, dserver also reports the failed
// command (map: incomplete result); either way the test fails, as there is
// no reason for an incomplete result here.
func TestDMapSlowClientGetsEveryGroup(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)

	inFile := "dmap_slow_client.log.tmp"
	csvFile := "dmap_slow_client.csv.tmp"
	outFile := "dmap_slow_client.stdout.tmp"
	cleanupFiles(t, inFile, csvFile, csvFile+".query", outFile)
	writeSlowClientInput(t, inFile)

	server := startSharedReadServer(t, sharedReadModes()[0], nil)
	// interval 3600: no periodic serialization runs during the test (the
	// default interval is 5 s, which a loaded host could reach before the
	// final one), so the final serialization sends every group itself.
	query := fmt.Sprintf("from STATS select count($line),user group by user interval 3600 outfile %s",
		csvFile)
	client := startSlowDMapClient(t, server.address, inFile, query, outFile)
	pauseDuringFinalResult(t, server, inFile, client)

	waitErr := client.Wait()
	if status := exitCodeFromError(waitErr); status != 0 {
		t.Fatalf("dmap exit status = %d, want 0 (%v)", status, waitErr)
	}
	server.requireLogCount(t, "Aggregate serialize interrupted", 0)
	server.requireLogCount(t, "final result incomplete", 0)
	verifySlowClientOutfile(t, csvFile)
}

// pauseDuringFinalResult stops the client as soon as dserver started reading
// inFile for it, so that it drains none of the result, waits until dserver's
// final serialization started (the query's interval keeps any periodic
// serialization from running first) and continues the client
// slowClientPause later. While the client is stopped the final serialization
// can send no more than the SSH channel window and the session's queues
// hold, so it is blocked for the whole pause.
func pauseDuringFinalResult(t *testing.T, server *sharedReadServer, inFile string, client *exec.Cmd) {
	t.Helper()
	server.waitForReaders(t, inFile, 1)
	if err := client.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop dmap: %v", err)
	}
	server.waitForLog(t, "Aggregate final serialization started", 1)
	// Every group is still to be sent: no periodic serialization took any
	// of them (and blocked on the stopped client) before the final one, whose
	// count would exclude them.
	server.requireLogCount(t, fmt.Sprintf("Aggregate final serialization started|groups|%d", slowClientGroups), 1)
	time.Sleep(slowClientPause)
	if err := client.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("continue dmap: %v", err)
	}
}

// writeSlowClientInput writes slowClientLinesPerGroup MAPREDUCE:STATS lines
// for each of slowClientGroups users.
func writeSlowClientInput(t *testing.T, path string) {
	t.Helper()
	fd, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	writer := bufio.NewWriter(fd)
	for i := range slowClientGroups * slowClientLinesPerGroup {
		_, _ = fmt.Fprintf(writer, "INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|"+
			"MAPREDUCE:STATS|currentConnections=%d|lifetimeConnections=%d|user=u%d\n",
			i%7, i, i%slowClientGroups)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := fd.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// startSlowDMapClient starts dmap against the server with stdout and stderr
// going to outFile. The process is killed when the test ends, which also
// ends a stopped one.
func startSlowDMapClient(t *testing.T, address, inFile, query, outFile string) *exec.Cmd {
	t.Helper()
	out, err := os.Create(outFile)
	if err != nil {
		t.Fatalf("create %s: %v", outFile, err)
	}
	defer closeIgnoringError(out)

	cmd := exec.Command("../dmap",
		"--cfg", writeFollowClientConfig(t),
		"--logger", "stdout",
		"--logLevel", "error",
		"--servers", address,
		"--files", inFile,
		"--trustAllHosts",
		"--noColor",
		"--query", query,
	)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dmap: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// verifySlowClientOutfile checks that the outfile has the header and one row
// per group, each with the group's full count.
func verifySlowClientOutfile(t *testing.T, csvFile string) {
	t.Helper()
	data, err := os.ReadFile(csvFile)
	if err != nil {
		t.Fatalf("read outfile: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if got, want := lines[0], "count($line),user"; got != want {
		t.Fatalf("outfile header = %q, want %q", got, want)
	}
	rows := lines[1:]
	if len(rows) != slowClientGroups {
		t.Fatalf("outfile has %d groups, want %d", len(rows), slowClientGroups)
	}
	wantCount := fmt.Sprintf("%d,", slowClientLinesPerGroup)
	users := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		count, user, ok := strings.Cut(row, ",")
		if !ok || count+"," != wantCount {
			t.Fatalf("outfile row %q, want count %d", row, slowClientLinesPerGroup)
		}
		users[user] = struct{}{}
	}
	if len(users) != slowClientGroups {
		t.Fatalf("outfile has %d distinct users, want %d", len(users), slowClientGroups)
	}
}
