package integrationtests

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this file check that reads dserver shares between sessions
// (internal/io/fs/readhub) give every session exactly the output of a
// private read: each scenario runs with shared reads on and with
// "SharedReadsDisable": true, requires byte-identical output, and checks the
// server's shared read log lines for the mode. The dtail clients' files hold
// lines matching their regexes before the clients start, which none of them
// may print: a follow read starts at the end of the file.

const (
	// Server log lines of the shared follow and one-shot reads.
	sharedFollowStartedLog  = "|Shared follow read started|"
	sharedFollowGainedLog   = "|Shared follow read gained a subscriber|subscribers="
	sharedFollowEvictedLog  = "|Shared follow read evicted a slow subscriber|"
	sharedFollowAnyLog      = "Shared follow read"
	sharedOneShotStartedLog = "|Shared one-shot read started|"
	sharedOneShotAnyLog     = "Shared one-shot read"
	truncatedLog            = "|File got truncated, reading from beginning"
	readingAgainLog         = "|Reading file again"
	noPermissionLog         = "|No permission to read file|"
	// sharedContinuousAttempts bounds how often the continuous fan-out test
	// appends its batch; see feedUntilOutfilesMatch.
	sharedContinuousAttempts = 5
)

// sharedContinuousJob is one continuous job of TestSharedReadContinuousFanOut.
type sharedContinuousJob struct {
	name  string
	query string
}

// TestSharedReadContinuousFanOut runs three continuous jobs with different
// queries on one file. With sharing on, dserver reads the file once for all
// three; every outfile must equal its .expected file in both modes.
func TestSharedReadContinuousFanOut(t *testing.T) {
	const input = "sharedread_continuous.log.tmp"
	jobs := []sharedContinuousJob{
		{"a", "from STATS select count($line),last($time),avg($goroutines),min(concurrentConnections)," +
			"max(lifetimeConnections) group by $hostname"},
		{"b", "from STATS select count($line),max(lifetimeConnections) group by $hostname " +
			"where lifetimeConnections >= 3"},
		{"c", "from STATS select count($time),$time,max($goroutines),avg($goroutines),min($goroutines) " +
			"group by $time order by count($time)"},
	}
	batch, err := os.ReadFile("mapr_testdata.log")
	if err != nil {
		t.Fatalf("read test data: %v", err)
	}

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		createEmptyFile(t, input)
		continuous := make([]map[string]any, 0, len(jobs))
		for _, job := range jobs {
			removeIgnoringError(sharedContinuousOutfile(job))
			removeIgnoringError(sharedContinuousOutfile(job) + ".query")
			cleanupFiles(t, sharedContinuousOutfile(job), sharedContinuousOutfile(job)+".query")
			continuous = append(continuous, map[string]any{
				"Name":      "sharedread_continuous_" + job.name,
				"Enable":    true,
				"AllowFrom": []string{"localhost"},
				"Files":     "./" + input,
				"Query":     job.query + " interval 1",
				"Outfile":   "./" + sharedContinuousOutfile(job),
			})
		}
		server := startSharedReadServer(t, mode, map[string]any{"Continuous": continuous})
		server.waitForReaders(t, input, len(jobs))

		outputs := feedUntilOutfilesMatch(t, input, string(batch), jobs)
		if mode.shared() {
			server.requireLogCount(t, sharedFollowStartedLog, 1)
			server.waitForLog(t, sharedFollowGainedLog+"3", 1)
		} else {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		return outputs
	})
}

// feedUntilOutfilesMatch appends batch to input in one write and waits until
// every job's outfile and .query file equal their .expected files. A
// continuous job's outfile holds its last non-empty interval, so a batch
// whose lines fall into two intervals, or one appended before a private
// reader sought to the end of the file, leaves a partial result; the batch
// is then appended again, up to sharedContinuousAttempts times. It returns
// the outfiles by job.
func feedUntilOutfilesMatch(t *testing.T, input, batch string, jobs []sharedContinuousJob) map[string]string {
	t.Helper()
	for attempt := 1; attempt <= sharedContinuousAttempts; attempt++ {
		appendToFile(t, input, batch)
		mismatch := waitForContinuousOutfiles(t, jobs)
		if mismatch == "" {
			outputs := make(map[string]string, len(jobs))
			for _, job := range jobs {
				outputs[job.name] = readTestFile(t, sharedContinuousOutfile(job))
			}
			return outputs
		}
		t.Logf("attempt %d: %s, appending the batch again", attempt, mismatch)
	}
	t.Fatalf("outfiles did not match their .expected files after %d batches", sharedContinuousAttempts)
	return nil
}

// waitForContinuousOutfiles waits a few intervals for the outfiles to match
// and describes the first mismatch, or returns "" when all match.
func waitForContinuousOutfiles(t *testing.T, jobs []sharedContinuousJob) string {
	t.Helper()
	const checks = 100 // 100 * sharedReadPoll = 5 intervals of 1s
	var mismatch string
	for range checks {
		mismatch = continuousOutfileMismatch(jobs)
		if mismatch == "" {
			return ""
		}
		time.Sleep(sharedReadPoll)
	}
	return mismatch
}

// continuousOutfileMismatch describes the first outfile or .query file that
// differs from its .expected file, or returns "".
func continuousOutfileMismatch(jobs []sharedContinuousJob) string {
	for _, job := range jobs {
		base := sharedContinuousBase(job)
		for _, suffix := range []string{"", ".query"} {
			got, err := os.ReadFile(base + ".tmp" + suffix)
			if err != nil {
				return err.Error()
			}
			want, err := os.ReadFile(base + suffix + ".expected")
			if err != nil {
				return err.Error()
			}
			if string(got) != string(want) {
				return fmt.Sprintf("%s.tmp%s differs from %s%s.expected", base, suffix, base, suffix)
			}
		}
	}
	return ""
}

// sharedContinuousBase is the name of the job's outfile without .tmp, and of
// its .expected files without .expected.
func sharedContinuousBase(job sharedContinuousJob) string {
	return "sharedread_continuous_" + job.name + ".csv"
}

// sharedContinuousOutfile is the job's outfile.
func sharedContinuousOutfile(job sharedContinuousJob) string {
	return sharedContinuousBase(job) + ".tmp"
}

// TestSharedReadScheduledFanOut runs the three scheduled jobs of dserver3.cfg
// on mapr_testdata.log. With sharing on they run as one group that dserver
// reads the file once for; with sharing off one at a time with private reads.
func TestSharedReadScheduledFanOut(t *testing.T) {
	cfg, err := os.ReadFile("dserver3.cfg")
	if err != nil {
		t.Fatalf("read dserver3.cfg: %v", err)
	}
	var base struct {
		Server map[string]any
	}
	if err := json.Unmarshal(cfg, &base); err != nil {
		t.Fatalf("parse dserver3.cfg: %v", err)
	}
	jobs := []string{"dserver3a", "dserver3b", "dserver3c"}

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		// A scheduled job does not run while its outfile exists.
		for _, job := range jobs {
			removeIgnoringError(job + ".csv.tmp")
			removeIgnoringError(job + ".csv.tmp.query")
		}
		server := startSharedReadServer(t, mode, base.Server)
		for _, job := range jobs {
			name := "dserver_schedule_group_test_" + strings.TrimPrefix(job, "dserver3")
			server.waitForLog(t, fmt.Sprintf("|Job %s exited with status 0", name), 1)
		}

		outputs := make(map[string]string)
		for _, job := range jobs {
			csvFile := job + ".csv.tmp"
			if err := compareFiles(t, csvFile, job+".csv.expected"); err != nil {
				t.Error(err)
			}
			if err := compareFiles(t, csvFile+".query", job+".csv.query.expected"); err != nil {
				t.Error(err)
			}
			outputs[csvFile] = readTestFile(t, csvFile)
			outputs[csvFile+".query"] = readTestFile(t, csvFile+".query")
		}
		if mode.shared() {
			server.requireLogCount(t, "|Starting job group of 3 jobs reading", 1)
			server.requireLogCount(t, sharedOneShotStartedLog, 1)
			server.requireLogCount(t, sharedOneShotStartedLog+"group=", 1)
			// The third member's join and the start of the read.
			server.requireLogCount(t, "|members=3/3", 2)
		} else {
			server.requireLogCount(t, "Starting job group", 0)
			server.requireLogCount(t, sharedOneShotAnyLog, 0)
		}
		return outputs
	})
}

// TestSharedReadMixedTail follows one file with a continuous job and two
// dtail clients with different regexes, one of them with before and after
// context. Both clients must print the same with sharing on and off. The
// continuous job, started with dserver, creates the shared reader the
// clients join, so the job's counts also check where that reader starts: at
// the end of the file, after the STATS lines the file holds beforehand.
func TestSharedReadMixedTail(t *testing.T) {
	const input = "sharedread_mixed.log.tmp"
	const outfile = "sharedread_mixed.csv.tmp"
	const plainRegex = "foo|SYNC|MARK"
	// The only STATS lines appended after the start: the job must count
	// exactly these.
	const finalStatsLines = 37
	var payload []string
	for i := range 300 {
		kind := "other"
		switch {
		case i%3 == 0:
			kind = "foo"
		case i%7 == 0:
			kind = "bar"
		}
		payload = append(payload, fmt.Sprintf("mixed %03d %s", i, kind))
	}
	payload = append(payload, "MARK end")

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		// The prefill lines are STATS lines, which the job counts should
		// its read start before them.
		createFileWithLines(t, input, sharedReadStatsLines(sharedReadPrefillMarker, sharedReadPrefillLines))
		removeIgnoringError(outfile)
		cleanupFiles(t, outfile, outfile+".query")
		server := startSharedReadServer(t, mode, map[string]any{"Continuous": []map[string]any{{
			"Name":      "sharedread_mixed",
			"Enable":    true,
			"AllowFrom": []string{"localhost"},
			"Files":     "./" + input,
			"Query":     "from STATS select count($line) group by $hostname interval 1",
			// Append mode keeps the count of every interval.
			"Outfile": "append ./" + outfile,
		}}})
		server.waitForReaders(t, input, 1)
		plain := startFollowClient(t, server, "mixed_plain", input, "--plain", "--grep", plainRegex)
		withContext := startFollowClient(t, server, "mixed_context", input,
			"--grep", "bar|SYNC|MARK", "--before", "2", "--after", "1")
		server.waitForReaders(t, input, 3)

		written := syncFollowClients(t, input, plain, withContext)
		appendFileLines(t, input, payload...)
		written = append(written, payload...)
		plain.waitForOutput(t, "MARK end")
		withContext.waitForOutput(t, "MARK end")

		outputs := map[string]string{
			"plain":   plain.outputWithoutSync(t),
			"context": withContext.outputAfterSync(t),
		}
		plain.requireNoMarkedLines(t, sharedReadPrefillMarker)
		withContext.requireNoMarkedLines(t, sharedReadPrefillMarker)
		if want := matchingLines(t, plainRegex, written); outputs["plain"] != want {
			t.Errorf("plain client output differs from the matching lines:\n%s",
				firstDifference(outputs["plain"], want))
		}

		// The clients' outputs were taken above. The job reads these lines
		// after every line before them, so once it counted all of them it
		// has counted every STATS line it read.
		appendFileLines(t, input, sharedReadStatsLines("FINAL", finalStatsLines)...)
		var counted int
		pollUntil(t, "the continuous job counting the appended STATS lines", func() bool {
			counted = continuousAppendedCount(t, outfile)
			return counted >= finalStatsLines
		})
		if counted != finalStatsLines {
			t.Errorf("continuous job counted %d STATS lines, want the %d appended after the start: "+
				"its read started before the end of the file", counted, finalStatsLines)
		}
		outputs["job"] = strconv.Itoa(counted)

		if mode.shared() {
			server.requireLogCount(t, sharedFollowStartedLog, 1)
			server.waitForLog(t, sharedFollowGainedLog+"3", 1)
		} else {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		return outputs
	})
}

// sharedReadStatsLines returns n STATS lines holding marker that match
// every client regex of the shared read tests.
func sharedReadStatsLines(marker string, n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|"+
			"lifetimeConnections=1|mark=%s %04d 7 foo bar", marker, i)
	}
	return lines
}

// continuousAppendedCount sums the counts of an append mode outfile of the
// query "select count($line) ...": the lines the job counted in all
// intervals so far. A missing outfile counts 0.
func continuousAppendedCount(t *testing.T, outfile string) int {
	t.Helper()
	data, err := os.ReadFile(outfile)
	if err != nil {
		return 0
	}
	var sum int
	for _, line := range strings.SplitAfter(string(data), "\n") {
		// Skip a row still being written; the header is no number.
		row, complete := strings.CutSuffix(line, "\n")
		if !complete {
			continue
		}
		if n, err := strconv.Atoi(row); err == nil {
			sum += n
		}
	}
	return sum
}

// TestSharedReadRotation rotates (move and create) and then copytruncates the
// file two dtail clients follow.
func TestSharedReadRotation(t *testing.T) {
	const input = "sharedread_rotate.log.tmp"
	const plainRegex = "foo|SYNC|MARK"

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		createPrefilledFile(t, input)
		cleanupFiles(t, input+".1", input+".2")
		server := startSharedReadServer(t, mode, nil)
		plain := startFollowClient(t, server, "rotate_plain", input, "--plain", "--grep", plainRegex)
		withContext := startFollowClient(t, server, "rotate_context", input,
			"--grep", "bar|SYNC|MARK", "--before", "1", "--after", "1")
		server.waitForReaders(t, input, 2)
		clients := []*followClient{plain, withContext}
		written := syncFollowClients(t, input, clients...)

		written = append(written, writeRotationPhase(t, input, "before", clients)...)
		// Rotation: the file is moved away and a new one created.
		if err := os.Rename(input, input+".1"); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		written = append(written, writeRotationPhase(t, input, "rotated", clients)...)
		// copytruncate: the file is copied and truncated in place. New lines
		// are written only once the readers noticed the truncation; before,
		// they could read on from their old offset.
		copyFile(t, input, input+".2")
		if err := os.Truncate(input, 0); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		readers := 2 // One per session.
		if mode.shared() {
			readers = 1
		}
		server.waitForLog(t, truncatedLog, readers)
		written = append(written, writeRotationPhase(t, input, "truncated", clients)...)

		outputs := map[string]string{
			"plain":   plain.outputWithoutSync(t),
			"context": withContext.outputAfterSync(t),
		}
		plain.requireNoMarkedLines(t, sharedReadPrefillMarker)
		withContext.requireNoMarkedLines(t, sharedReadPrefillMarker)
		if want := matchingLines(t, plainRegex, written); outputs["plain"] != want {
			t.Errorf("plain client output differs from the matching lines:\n%s",
				firstDifference(outputs["plain"], want))
		}
		server.requireLogCount(t, truncatedLog, readers)
		if mode.shared() {
			server.requireLogCount(t, sharedFollowStartedLog, 1)
			server.requireLogCount(t, sharedFollowGainedLog+"2", 1)
			server.requireLogCount(t, sharedFollowEvictedLog, 0)
		} else {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		return outputs
	})
}

// TestSharedReadLateJoiner starts a second dtail client while the shared
// reader lags behind the file: the path was rotated to a new file that
// already holds lines, and the reader, still on the old file, reads the new
// one only after its retry interval. A private read opened at the join starts
// at the end of the new file, so the late client must not print the lines
// that were in it before it joined; the first client prints them, as it
// reads the new file from its beginning.
func TestSharedReadLateJoiner(t *testing.T) {
	const input = "sharedread_latejoin.log.tmp"
	const regex = "foo|SYNC|MARK"
	const preJoinMarker = "PREJOIN"
	// The reader reopens the path this long after it noticed the rotation;
	// the late client joins in between.
	const retryIntervalMs = 5000

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		createPrefilledFile(t, input)
		cleanupFiles(t, input+".1")
		server := startSharedReadServer(t, mode, map[string]any{"ReadRetryIntervalMs": retryIntervalMs})
		first := startFollowClient(t, server, "latejoin_first", input, "--plain", "--grep", regex)
		server.waitForReaders(t, input, 1)
		written := syncFollowClients(t, input, first)
		written = append(written, writeRotationPhase(t, input, "early", []*followClient{first})...)

		if err := os.Rename(input, input+".1"); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		preJoin := markedLines(preJoinMarker, 200)
		createFileWithLines(t, input, preJoin)
		written = append(written, preJoin...)

		late := startFollowClient(t, server, "latejoin_late", input, "--plain", "--grep", regex)
		if mode.shared() {
			server.waitForLog(t, sharedFollowGainedLog+"2", 1)
			if server.logs.count(readingAgainLog) > 0 {
				t.Fatalf("the late client joined after the shared reader reopened the file, "+
					"more than %d ms after the rotation; the join must fall into the lag", retryIntervalMs)
			}
		} else {
			server.waitForReaders(t, input, 2)
		}
		clients := []*followClient{first, late}
		afterJoin := syncFollowClients(t, input, clients...)
		afterJoin = append(afterJoin, writeRotationPhase(t, input, "late", clients)...)
		written = append(written, afterJoin...)

		outputs := map[string]string{
			"first": first.outputWithoutSync(t),
			"late":  late.outputWithoutSync(t),
		}
		for _, client := range clients {
			client.requireNoMarkedLines(t, sharedReadPrefillMarker)
		}
		late.requireNoMarkedLines(t, preJoinMarker)
		if want := matchingLines(t, regex, written); outputs["first"] != want {
			t.Errorf("first client output differs from the matching lines:\n%s",
				firstDifference(outputs["first"], want))
		}
		if want := matchingLines(t, regex, afterJoin); outputs["late"] != want {
			t.Errorf("late client output differs from the lines matching after its join:\n%s",
				firstDifference(outputs["late"], want))
		}
		if mode.shared() {
			server.requireLogCount(t, sharedFollowStartedLog, 1)
			server.requireLogCount(t, sharedFollowEvictedLog, 0)
		} else {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		return outputs
	})
}

// TestSharedReadEvictsStalledClient stops one of two dtail clients
// (SIGSTOP) during a burst far larger than a shared subscriber's queue. The
// other client must get its lines while the first is stopped, and the
// stopped one, once resumed, all of its lines: with sharing on it is evicted
// to a private reader that continues after its last line.
func TestSharedReadEvictsStalledClient(t *testing.T) {
	const input = "sharedread_evict.log.tmp"
	const stalledRegex = "foo|SYNC|MARK"
	const burstLines = 200000
	var burst strings.Builder
	for i := range burstLines {
		fmt.Fprintf(&burst, "%07d foo %s\n", i, strings.Repeat("x", 110))
	}

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		createPrefilledFile(t, input)
		server := startSharedReadServer(t, mode, nil)
		stalled := startFollowClient(t, server, "evict_stalled", input, "--plain", "--grep", stalledRegex)
		fast := startFollowClient(t, server, "evict_fast", input, "--grep", "7 foo|SYNC|MARK")
		server.waitForReaders(t, input, 2)
		written := syncFollowClients(t, input, stalled, fast)

		stalled.signal(t, syscall.SIGSTOP)
		appendToFile(t, input, burst.String())
		appendFileLines(t, input, "MARK fast")
		// The fast client is not held up by the stopped one.
		fast.waitForOutput(t, "MARK fast")
		if mode.shared() {
			server.waitForLog(t, sharedFollowEvictedLog, 1)
		}
		stalled.signal(t, syscall.SIGCONT)
		appendFileLines(t, input, "MARK end")
		waitForClients(t, "MARK end", stalled, fast)

		outputs := map[string]string{
			"stalled": stalled.outputWithoutSync(t),
			"fast":    fast.outputAfterSync(t),
		}
		written = append(written, strings.Split(strings.TrimSuffix(burst.String(), "\n"), "\n")...)
		written = append(written, "MARK fast", "MARK end")
		stalled.requireNoMarkedLines(t, sharedReadPrefillMarker)
		fast.requireNoMarkedLines(t, sharedReadPrefillMarker)
		if want := matchingLines(t, stalledRegex, written); outputs["stalled"] != want {
			t.Errorf("stalled client output differs from the matching lines:\n%s",
				firstDifference(outputs["stalled"], want))
		}
		if !mode.shared() {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		return outputs
	})
}

// TestSharedReadPermissionDenied tails a file with a dtail client whose user
// may not read it while a continuous job follows the file through a shared
// reader: the client gets the usual denial and none of the lines.
func TestSharedReadPermissionDenied(t *testing.T) {
	const input = "sharedread_denied.log.tmp"
	const outfile = "sharedread_denied.csv.tmp"
	const payload = "INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|lifetimeConnections=1"

	runSharedReadModes(t, func(t *testing.T, mode sharedReadMode) map[string]string {
		createEmptyFile(t, input)
		removeIgnoringError(outfile)
		cleanupFiles(t, outfile, outfile+".query")
		server := startSharedReadServer(t, mode, map[string]any{
			"Permissions": map[string]any{
				"Users": map[string][]string{
					currentUsername(t): {"readfiles:^/nonexistent/sharedread$"},
				},
			},
			"Continuous": []map[string]any{{
				"Name":      "sharedread_denied",
				"Enable":    true,
				"AllowFrom": []string{"localhost"},
				"Files":     "./" + input,
				"Query":     "from STATS select count($line) group by $hostname interval 1",
				"Outfile":   "./" + outfile,
			}},
		})
		server.waitForReaders(t, input, 1)
		if mode.shared() {
			server.waitForLog(t, sharedFollowStartedLog, 1)
		}

		client := startFollowClient(t, server, "denied", input, "--plain", "--grep", "STATS")
		server.waitForLog(t, noPermissionLog, 1)
		client.waitForOutput(t, "Unable to read file(s), check server logs")

		// Append until the job reported lines: they went through the reader
		// the denied client would have joined.
		pollUntil(t, "the continuous job reporting lines", func() bool {
			appendFileLines(t, input, payload)
			data, err := os.ReadFile(outfile)
			return err == nil && strings.Count(string(data), "\n") >= 2
		})
		if strings.Contains(client.output(t), "MAPREDUCE:STATS") {
			t.Errorf("denied client printed lines of the file:\n%s", client.output(t))
		}
		server.requireLogCount(t, "|Start reading|"+input+"|", 1)
		if mode.shared() {
			server.requireLogCount(t, sharedFollowStartedLog, 1)
			server.requireLogCount(t, sharedFollowGainedLog, 0)
		} else {
			server.requireLogCount(t, sharedFollowAnyLog, 0)
		}
		return map[string]string{"denied": withoutLogPrefixes(client.output(t))}
	})
}

// waitForClients waits until every client printed marker.
func waitForClients(t *testing.T, marker string, clients ...*followClient) {
	t.Helper()
	for _, client := range clients {
		client.waitForOutput(t, marker)
	}
}

// copyFile copies src to dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// withoutLogPrefixes removes the time and the session (user, address and
// port) from client log lines such as
// WARN|0922-031210|user@127.0.0.1:44684|message: they differ from run to run.
func withoutLogPrefixes(output string) string {
	lines := strings.SplitAfter(output, "\n")
	for i, line := range lines {
		fields := strings.SplitN(line, "|", 4)
		if len(fields) == 4 && (fields[0] == "INFO" || fields[0] == "WARN" || fields[0] == "ERROR") {
			lines[i] = fields[0] + "|" + fields[3]
		}
	}
	return strings.Join(lines, "")
}

// writeRotationPhase appends 100 lines named after the phase and a marker
// line, waits until every client printed the marker and returns the lines.
func writeRotationPhase(t *testing.T, file, phase string, clients []*followClient) []string {
	t.Helper()
	lines := make([]string, 0, 101)
	for i := range 100 {
		kind := "other"
		switch {
		case i%2 == 0:
			kind = "foo"
		case i%5 == 0:
			kind = "bar"
		}
		lines = append(lines, fmt.Sprintf("%s %03d %s", phase, i, kind))
	}
	marker := "MARK " + phase
	lines = append(lines, marker)
	appendFileLines(t, file, lines...)
	waitForClients(t, marker, clients...)
	return lines
}
