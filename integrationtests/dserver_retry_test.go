package integrationtests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// TestDServerScheduledRetryAfterFailure runs a scheduled job twice, as two
// dserver runs within the same date period. In the first run the job's server
// is not reachable (connection refused): the job fails and must leave no
// outfile, .query or temporary file behind, so it is not skipped for the rest
// of the period. In the second run dserver listens on the job's server port:
// the job runs again and writes the same outfile a job that succeeds at once
// writes (dserver1.csv.expected, same query and data).
func TestDServerScheduledRetryAfterFailure(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDServerScheduledRetryAfterFailure")
	defer writeLogFileIgnoringError(testLogger)

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := WithTestLogger(baseCtx, testLogger)

	const (
		cfgFile   = "dserver4.cfg.tmp"
		csvFile   = "dserver4.csv.tmp"
		queryFile = csvFile + ".query"
		query     = "from STATS select count($line),last($time),avg($goroutines)," +
			"min(concurrentConnections),max(lifetimeConnections) group by $hostname"
	)
	jobPort := getUniquePortNumber()
	otherPort := getUniquePortNumber()
	cfg := fmt.Sprintf(`{"Server": {"Schedule": [{
  "Name": "dserver_schedule_retry_test", "Enable": true,
  "AllowFrom": ["localhost"], "TimeRange": [0, 24],
  "Servers": ["127.0.0.1:%d"], "Files": "./mapr_testdata.log",
  "Query": %q, "Outfile": "./%s"}]}}`, jobPort, query, csvFile)
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// cleanupTmpFiles leaves the .query file of an earlier run of this test.
	if err := os.Remove(queryFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	// First run: nothing listens on the job's server port.
	output := runScheduledDServer(ctx, t, cfgFile, otherPort, 5)
	if !strings.Contains(output, "Job dserver_schedule_retry_test failed and wrote no outfile") {
		t.Errorf("first run did not log the failed job, output:\n%s", output)
	}
	for _, path := range []string{csvFile, queryFile, csvFile + ".tmp", queryFile + ".tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed job left %s (stat error %v)", path, err)
		}
	}

	// Second run: dserver itself is the job's server.
	output = runScheduledDServer(ctx, t, cfgFile, jobPort, 10)
	if strings.Contains(output, "Not running job as outfile already exists") {
		t.Errorf("second run skipped the job, output:\n%s", output)
	}
	if err := compareFilesWithContext(ctx, t, csvFile, "dserver1.csv.expected"); err != nil {
		t.Error(err)
	}
	if err := compareFilesWithContext(ctx, t, queryFile, "dserver4.csv.query.expected"); err != nil {
		t.Error(err)
	}
}

// runScheduledDServer runs dserver with cfgFile on port until it shuts down
// after shutdownAfter seconds and returns its standard output.
func runScheduledDServer(ctx context.Context, t *testing.T, cfgFile string, port, shutdownAfter int) string {
	t.Helper()
	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", cfgFile,
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", "127.0.0.1",
		"--shutdownAfter", fmt.Sprintf("%d", shutdownAfter),
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	for {
		select {
		case line, ok := <-stdoutCh:
			if ok {
				t.Log(line)
				output.WriteString(line)
				output.WriteString("\n")
			}
		case line, ok := <-stderrCh:
			if ok {
				t.Log(line)
			}
		case cmdErr := <-cmdErrCh:
			t.Logf("dserver finished with exit code %d: %v", exitCodeFromError(cmdErr), cmdErr)
			return output.String()
		}
	}
}

// TestDServerScheduledRemoteReadFailures runs a scheduled job of one dserver
// (A) against another dserver (B) three times, each as a dserver run of its
// own, within the same date period. The job reads a file of today that does
// not exist at first:
//
//  1. B is killed (SIGKILL) while it reads for the job: the session ends
//     without B's close handshake, so the job fails and leaves no outfile.
//  2. B completes the session, but reports the read as failed (no file to
//     read): the job fails and leaves no outfile, not even a header-only one.
//  3. The file exists now: the job writes the same outfile a job that
//     succeeds at once writes (dserver1.csv.expected, same query and data).
func TestDServerScheduledRemoteReadFailures(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDServerScheduledRemoteReadFailures")
	defer writeLogFileIgnoringError(testLogger)

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := WithTestLogger(baseCtx, testLogger)

	const (
		cfgFileA  = "dserver5a.cfg.tmp"
		cfgFileB  = "dserver5b.cfg.tmp"
		csvFile   = "dserver5.csv.tmp"
		queryFile = csvFile + ".query"
		name      = "dserver_schedule_remote_test"
		query     = "from STATS select count($line),last($time),avg($goroutines)," +
			"min(concurrentConnections),max(lifetimeConnections) group by $hostname"
	)
	dataFile := "dserver5-" + time.Now().Format("20060102") + ".log.tmp"
	portA := getUniquePortNumber()
	portB := getUniquePortNumber()
	job := func(enable bool, servers string) string {
		return fmt.Sprintf(`{"Server": {"Schedule": [{
  "Name": %q, "Enable": %v, "AllowFrom": ["localhost"], "TimeRange": [0, 24],
  %s "Files": "./dserver5-$today.log.tmp", "Query": %q, "Outfile": "./%s"}]}}`,
			name, enable, servers, query, csvFile)
	}
	if err := os.WriteFile(cfgFileA, []byte(job(true, fmt.Sprintf(`"Servers": ["127.0.0.1:%d"],`, portB))), 0o600); err != nil {
		t.Fatal(err)
	}
	// B knows the job (disabled) so that A's scheduler user may log in.
	if err := os.WriteFile(cfgFileB, []byte(job(false, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{queryFile, dataFile} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	assertNoOutfile := func(phase string) {
		t.Helper()
		for _, path := range []string{csvFile, queryFile, csvFile + ".tmp", queryFile + ".tmp"} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: failed job left %s (stat error %v)", phase, path, err)
			}
		}
	}

	// 1. B is killed while it waits for the file to appear.
	bCtx, killB := context.WithCancel(ctx)
	b := startServerB(bCtx, t, cfgFileB, portB, "No such file(s) to read", killB)
	output := runScheduledDServer(ctx, t, cfgFileA, portA, 8)
	killB()
	<-b
	if !strings.Contains(output, "ended before the server completed it") ||
		!strings.Contains(output, "Job "+name+" failed and wrote no outfile") {
		t.Errorf("run with a killed server did not log the incomplete session, output:\n%s", output)
	}
	assertNoOutfile("killed server")

	// 2. B gives up reading the file that does not exist yet.
	bCtx, killB = context.WithCancel(ctx)
	b = startServerB(bCtx, t, cfgFileB, portB, "", nil)
	output = runScheduledDServer(ctx, t, cfgFileA, portA, 12)
	killB()
	<-b
	if !strings.Contains(output, "files could not be read") || !strings.Contains(output, ": read: no file to read") ||
		!strings.Contains(output, "Job "+name+" failed and wrote no outfile") {
		t.Errorf("run without the file did not log the failed read, output:\n%s", output)
	}
	assertNoOutfile("missing file")

	// 3. The file exists.
	data, err := os.ReadFile("mapr_testdata.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bCtx, killB = context.WithCancel(ctx)
	b = startServerB(bCtx, t, cfgFileB, portB, "", nil)
	output = runScheduledDServer(ctx, t, cfgFileA, portA, 10)
	killB()
	<-b
	if strings.Contains(output, "failed and wrote no outfile") {
		t.Errorf("run with the file failed, output:\n%s", output)
	}
	if err := compareFilesWithContext(ctx, t, csvFile, "dserver1.csv.expected"); err != nil {
		t.Error(err)
	}
	if err := os.Remove(queryFile); err != nil {
		t.Error(err)
	}
}

// startServerB starts the dserver a scheduled job reads from on port. When
// its output has a line containing killOn, it calls kill, which is to cancel
// ctx and so SIGKILL it. The returned channel is closed once it exited.
func startServerB(ctx context.Context, t *testing.T, cfgFile string, port int, killOn string,
	kill func()) <-chan struct{} {
	t.Helper()
	stdoutCh, stderrCh, cmdErrCh, err := startCommand(ctx, t,
		"", "../dserver",
		"--cfg", cfgFile,
		"--logger", "stdout",
		"--logLevel", "info",
		"--bindAddress", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
	)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case line, ok := <-stdoutCh:
				if ok {
					t.Log("B:", line)
					if killOn != "" && strings.Contains(line, killOn) {
						t.Log("B: killing it")
						kill()
					}
				}
			case line, ok := <-stderrCh:
				if ok {
					t.Log("B:", line)
				}
			case cmdErr := <-cmdErrCh:
				t.Logf("B: dserver exited: %v", cmdErr)
				return
			}
		}
	}()
	return exited
}

// TestDServerScheduledGlobWithSubdirectoryAndMissingFile runs two scheduled
// jobs within their TimeRange with one dserver that is their server:
//   - "dirglob" reads a glob that matches the test data and a directory. The
//     directory is not read and is no failure: the job writes the same outfile
//     as a job reading the test data alone (dserver1.csv.expected).
//   - "mixlist" reads a list of the test data and a file that does not exist.
//     Within its TimeRange the job writes no outfile at all.
func TestDServerScheduledGlobWithSubdirectoryAndMissingFile(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDServerScheduledGlobWithSubdirectoryAndMissingFile")
	defer writeLogFileIgnoringError(testLogger)

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := WithTestLogger(baseCtx, testLogger)

	const (
		cfgFile = "dserver6.cfg.tmp"
		dirCSV  = "dserver6-dirglob.csv.tmp"
		listCSV = "dserver6-mixlist.csv.tmp"
		query   = "from STATS select count($line),last($time),avg($goroutines)," +
			"min(concurrentConnections),max(lifetimeConnections) group by $hostname"
	)
	dataDir := t.TempDir()
	data, err := os.ReadFile("mapr_testdata.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "mapr_testdata.log"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dataDir, "archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	port := getUniquePortNumber()
	job := func(name, files, outfile string) string {
		return fmt.Sprintf(`{"Name": %q, "Enable": true, "AllowFrom": ["localhost"], "TimeRange": [0, 24],
  "Files": %q, "Query": %q, "Outfile": "./%s"}`, name, files, query, outfile)
	}
	cfg := fmt.Sprintf(`{"Server": {"Schedule": [%s, %s]}}`,
		job("dirglob", filepath.Join(dataDir, "*"), dirCSV),
		job("mixlist", "./mapr_testdata.log,"+filepath.Join(dataDir, "missing.log"), listCSV))
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dirCSV + ".query", listCSV + ".query"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	output := runScheduledDServer(ctx, t, cfgFile, port, 12)

	if strings.Contains(output, "Job dirglob failed") {
		t.Errorf("the job reading a glob with a directory failed, output:\n%s", output)
	}
	if err := compareFilesWithContext(ctx, t, dirCSV, "dserver1.csv.expected"); err != nil {
		t.Error(err)
	}
	if err := os.Remove(dirCSV + ".query"); err != nil {
		t.Error(err)
	}
	if !strings.Contains(output, "Job mixlist failed and wrote no outfile") ||
		!strings.Contains(output, ": read: no file to read") {
		t.Errorf("the job reading a missing file did not log its failure, output:\n%s", output)
	}
	for _, path := range []string{listCSV, listCSV + ".query", listCSV + ".tmp", listCSV + ".query.tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the job reading a missing file left %s (stat error %v)", path, err)
		}
	}
}
