package clients

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// pausableLogger records its diagnostics and, while paused, blocks every
// one of them, as the client logger does while the terminal output is
// paused (e.g. for the host key question).
type pausableLogger struct {
	clientlog.NopLogger
	debug bool

	mu      sync.Mutex
	resumed *sync.Cond
	paused  bool
	lines   []string
}

func newPausableLogger() *pausableLogger {
	l := &pausableLogger{}
	l.resumed = sync.NewCond(&l.mu)
	return l
}

func (l *pausableLogger) setPaused(paused bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = paused
	l.resumed.Broadcast()
}

func (l *pausableLogger) log(level string, args []any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.paused {
		l.resumed.Wait()
	}
	l.lines = append(l.lines, level+" "+strings.TrimSpace(fmt.Sprintln(args...)))
	return ""
}

func (l *pausableLogger) Error(args ...any) string { return l.log("ERROR", args) }
func (l *pausableLogger) Warn(args ...any) string  { return l.log("WARN", args) }
func (l *pausableLogger) Info(args ...any) string  { return l.log("INFO", args) }
func (l *pausableLogger) Debug(args ...any) string { return l.log("DEBUG", args) }
func (l *pausableLogger) DebugEnabled() bool       { return l.debug }

// Raw and RawLog block while paused too, as the payload of the client
// logger does.
func (l *pausableLogger) Raw(message string) string    { return l.log("RAW", []any{message}) }
func (l *pausableLogger) RawLog(message string) string { return l.log("RAWLOG", []any{message}) }

func (l *pausableLogger) logged() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// within fails the test unless fn returns within a generous time.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked behind the paused logger", what)
	}
}

// TestDeferredLoggerNeverBlocksItsCaller: diagnostics logged while the
// target is paused return at once and are logged in order once it runs
// again; flush waits for them. Debug diagnostics are not queued while debug
// logging is off.
func TestDeferredLoggerNeverBlocksItsCaller(t *testing.T) {
	target := newPausableLogger()
	deferred := newDeferredLogger(target)
	target.setPaused(true)
	within(t, "logging", func() {
		deferred.Info("one")
		deferred.Debug("not logged")
		deferred.Warn("two", 2)
		deferred.Error("three")
	})
	if got := target.logged(); len(got) != 0 {
		t.Fatalf("logged %q while the target was paused", got)
	}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		deferred.flush()
	}()
	select {
	case <-flushed:
		t.Fatal("flush returned while diagnostics were queued")
	case <-time.After(50 * time.Millisecond):
	}

	target.setPaused(false)
	<-flushed
	want := []string{"INFO one", "WARN two 2", "ERROR three"}
	if got := target.logged(); !slices.Equal(got, want) {
		t.Fatalf("logged %q, want %q", got, want)
	}

	target.debug = true
	deferred.Debug("four")
	deferred.flush()
	if got := target.logged(); len(got) != 4 || got[3] != "DEBUG four" {
		t.Fatalf("logged %q, want the debug line last", got)
	}
	deferred.flush() // nothing queued: returns at once
}

// TestDeferredLoggerDropsBeyondItsQueue: a target that takes nothing for a
// long time does not make the queue grow without bound; what did not fit is
// dropped and its number logged.
func TestDeferredLoggerDropsBeyondItsQueue(t *testing.T) {
	target := newPausableLogger()
	deferred := newDeferredLogger(target)
	target.setPaused(true)
	// The first diagnostic is taken off the queue by the drainer, which
	// then blocks in the target; wait for that, so the count is exact.
	deferred.Info("first")
	for {
		deferred.mu.Lock()
		taken := len(deferred.queue) == 0
		deferred.mu.Unlock()
		if taken {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for i := range deferredLogMax + 7 {
		deferred.Info("line", i)
	}
	target.setPaused(false)
	deferred.flush()

	got := target.logged()
	if len(got) != deferredLogMax+2 {
		t.Fatalf("logged %d lines, want the first, %d queued and the drop report", len(got), deferredLogMax)
	}
	if last := got[len(got)-1]; !strings.HasPrefix(last, "WARN Dropped mapreduce diagnostics") ||
		!strings.HasSuffix(last, " 7") {
		t.Fatalf("last line %q, want the report of 7 dropped diagnostics", last)
	}
}

// TestMaprReportUnderPausedLoggerDoesNotBlockReload is the regression test
// of a deadlock of an interactive dmap with an outfile: the host key
// question of a server still connecting came up while the control loop ran
// a :reload, and paused the client logger. The reporter then blocked in
// the paused logger ("Writing to outfile ...") with the session state's
// read lock and the global group set's lock held, the reload's commit
// waited for the session state for good, and nobody was left to answer the
// question. Now the report and the commit both finish while the logger is
// paused, and the diagnostics follow once it runs again.
func TestMaprReportUnderPausedLoggerDoesNotBlockReload(t *testing.T) {
	logger := newPausableLogger()
	outfile := filepath.Join(t.TempDir(), "out.csv")
	rawQuery := "from STATS select count(foo) group by foo interval 20 outfile " + outfile
	query := mustMaprClientQuery(t, rawQuery)
	c := &MaprClient{
		baseClient: baseClient{mu: newBaseClientMu(), Args: config.Args{Mode: omode.MapClient}},
		mode:       DefaultMode,
	}
	c.session, c.resultLog = newMaprSession(query, logger)
	c.loggers = LoggerDependencies{Client: logger}.normalized()
	sendReportTestAggregate(t, c, "srv1", 3)

	logger.setPaused(true)
	within(t, "the interim report", func() {
		if err := c.reportResults(false); err != nil {
			t.Errorf("reportResults() error = %v", err)
		}
	})
	within(t, "the reload's commit", func() {
		c.session.Switching(true)
		if _, err := c.session.CommitQuery(rawQuery, 2); err != nil {
			t.Errorf("CommitQuery() error = %v", err)
		}
		c.session.Switching(false)
	})
	if got := logger.logged(); len(got) != 0 {
		t.Fatalf("logged %q while the logger was paused", got)
	}

	logger.setPaused(false)
	c.flushResultLog()
	got := strings.Join(logger.logged(), "\n")
	for _, want := range []string{"INFO Writing to outfile " + outfile, "INFO Successfully renamed outfile to " + outfile} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
}

// TestMaprClientWithoutResultLogKeepsClientLogger: a client built without
// the deferred logger (tests) logs through its client logger, and its
// flush does nothing.
func TestMaprClientWithoutResultLogKeepsClientLogger(t *testing.T) {
	logger := newPausableLogger()
	c := &MaprClient{baseClient: baseClient{mu: newBaseClientMu()}}
	c.loggers = LoggerDependencies{Client: logger}.normalized()
	c.lockedLogger().Info("direct")
	c.flushResultLog()
	if got := logger.logged(); len(got) != 1 || got[0] != "INFO direct" {
		t.Fatalf("logged %q, want the line through the client logger", got)
	}
}

// TestMaprTailSwitchUnderPausedLoggerDoesNotBlockControlLoop is the
// regression test of the same deadlock without any lock or outfile: :tail
// ends the running query on the control loop (commitSessionSpec,
// dropQuery), which prints the query's last table. With the client logger
// paused by a host key question asked during the reload, the control loop
// waited in the logger, and nobody was left to answer the question. Now
// the table is rendered at once and printed once the logger runs again, in
// order, and never dropped.
func TestMaprTailSwitchUnderPausedLoggerDoesNotBlockControlLoop(t *testing.T) {
	logger := newPausableLogger()
	query := mustMaprClientQuery(t, "from STATS select count(foo) group by foo interval 20")
	c := &MaprClient{
		baseClient: baseClient{mu: newBaseClientMu(), Args: config.Args{Mode: omode.TailClient}},
		mode:       DefaultMode,
	}
	c.profile.switchWorkload = true
	c.session, c.resultLog = newMaprSession(query, logger)
	c.loggers = LoggerDependencies{Client: logger}.normalized()
	c.runtime = newClientRuntimeBoundary(clientTestRuntimeConfig(), c.loggers)
	sendReportTestAggregate(t, c, "srv1", 3)

	logger.setPaused(true)
	within(t, "the switch back to following lines", func() {
		c.session.Switching(true)
		if err := c.commitSessionSpec(SessionSpec{}, 2); err != nil {
			t.Errorf("commitSessionSpec() error = %v", err)
		}
		c.session.Switching(false)
	})
	if c.session.Snapshot().Query != nil {
		t.Fatal("the query is still active after the switch")
	}
	// More diagnostics than the queue keeps must not push the table out.
	for i := range deferredLogMax + 5 {
		c.controlLogger().Info("filler", i)
	}
	if got := logger.logged(); len(got) != 0 {
		t.Fatalf("logged %q while the logger was paused", got)
	}

	logger.setPaused(false)
	c.flushResultLog()
	lines := logger.logged()
	var raw []string
	for _, line := range lines {
		if strings.HasPrefix(line, "RAW ") {
			raw = append(raw, line)
		}
	}
	if len(raw) != 2 || !strings.Contains(raw[0], "from STATS select count(foo)") ||
		!strings.Contains(raw[1], "count(foo)") || !strings.Contains(raw[1], "3") {
		t.Fatalf("payload %q, want the query and then its last table with count 3", raw)
	}
	if !strings.HasPrefix(lines[0], "RAW ") {
		t.Fatalf("first line %q, want the table before the later diagnostics", lines[0])
	}
}

// TestMaprReporterAndFinishStaySynchronous: only the control loop's output
// is deferred. The reporter and the final result print through the client
// logger itself, so a table is on the terminal when the report returns.
func TestMaprReporterAndFinishStaySynchronous(t *testing.T) {
	logger := newPausableLogger()
	query := mustMaprClientQuery(t, "from STATS select count(foo) group by foo interval 20")
	c := &MaprClient{
		baseClient: baseClient{mu: newBaseClientMu(), Args: config.Args{Mode: omode.MapClient}},
		mode:       DefaultMode,
	}
	c.session, c.resultLog = newMaprSession(query, logger)
	c.loggers = LoggerDependencies{Client: logger}.normalized()
	c.runtime = newClientRuntimeBoundary(clientTestRuntimeConfig(), c.loggers)
	sendReportTestAggregate(t, c, "srv1", 3)

	if err := c.reportResults(true); err != nil {
		t.Fatalf("reportResults() error = %v", err)
	}
	if got := logger.logged(); len(got) != 2 || !strings.HasPrefix(got[1], "RAW ") {
		t.Fatalf("logged %q right after the report, want the query and the table", got)
	}
}
