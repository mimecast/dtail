package clients

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/protocol"
)

func TestIncompleteQueryReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ctxErr   error
		status   int
		complete bool
	}{
		{name: "completed", complete: true},
		{name: "failed connection", status: 1},
		{name: "failed connection with other status", status: 3},
		{name: "canceled", ctxErr: context.Canceled},
		{name: "deadline exceeded", ctxErr: context.DeadlineExceeded},
		{name: "failed and canceled", ctxErr: context.Canceled, status: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason := incompleteQueryReason(tt.ctxErr, tt.status)
			if (reason == "") != tt.complete {
				t.Fatalf("incompleteQueryReason(%v, %d) = %q, want complete=%v",
					tt.ctxErr, tt.status, reason, tt.complete)
			}
		})
	}
}

// TestMaprClientFinishWritesScheduledResultOnlyWhenComplete covers the final
// write decision: a scheduled query writes its outfile and .query file only
// when it completed and then returns 0; otherwise it writes nothing, keeps an
// outfile from an earlier run and returns a non-zero status. Other cumulative
// clients keep writing their final result whatever the status.
//
// Unless a case gives outcomes, the client has one connection whose session
// the server completed.
func TestMaprClientFinishWritesScheduledResultOnlyWhenComplete(t *testing.T) {
	t.Parallel()

	const previous = "previous result\n"
	completed := handlers.SessionOutcome{Completed: true}
	cutShort := handlers.SessionOutcome{}
	failedRead := handlers.SessionOutcome{Completed: true, Failures: []string{protocol.CommandFailureNoFile}}
	unreadable := handlers.SessionOutcome{Completed: true, Failures: []string{
		protocol.CommandFailurePermission, protocol.CommandFailureReadingFile, protocol.CommandFailureReader,
		protocol.CommandFailureGlobCapped}}
	denied := handlers.SessionOutcome{Completed: true, Failures: []string{protocol.CommandFailurePermission}}
	failedQuery := handlers.SessionOutcome{Completed: true, Failures: []string{protocol.CommandFailureMapQuery}}
	failedReadAndQuery := handlers.SessionOutcome{Completed: true, Failures: []string{
		protocol.CommandFailureNoFile, protocol.CommandFailureRejected}}
	failedReadCutShort := handlers.SessionOutcome{Failures: []string{protocol.CommandFailureNoFile}}
	tests := []struct {
		name       string
		mode       MaprClientMode
		status     int
		ctxErr     error
		outcomes   []handlers.SessionOutcome
		noOutcome  bool
		existing   bool
		badDir     bool
		wantStatus int
		wantResult bool
		// wantLog is a warning the client must log, its arguments joined
		// with "|".
		wantLog string
	}{
		{name: "scheduled completed", mode: ScheduledMode, wantResult: true},
		{name: "scheduled completed replaces earlier outfile", mode: ScheduledMode, existing: true, wantResult: true},
		{name: "scheduled failed", mode: ScheduledMode, status: 1, wantStatus: 1},
		{name: "scheduled failed keeps its status", mode: ScheduledMode, status: 3, wantStatus: 3},
		{name: "scheduled failed keeps earlier outfile", mode: ScheduledMode, status: 1, existing: true, wantStatus: 1},
		{name: "scheduled canceled", mode: ScheduledMode, ctxErr: context.Canceled, wantStatus: 1},
		{name: "scheduled canceled keeps earlier outfile", mode: ScheduledMode, ctxErr: context.Canceled,
			existing: true, wantStatus: 1},
		{name: "scheduled write error", mode: ScheduledMode, badDir: true, wantStatus: 1},
		{name: "scheduled two servers completed", mode: ScheduledMode,
			outcomes: []handlers.SessionOutcome{completed, completed}, wantResult: true},
		{name: "scheduled session cut short", mode: ScheduledMode, outcomes: []handlers.SessionOutcome{cutShort},
			wantStatus: 1},
		{name: "scheduled second session cut short", mode: ScheduledMode,
			outcomes: []handlers.SessionOutcome{completed, cutShort}, wantStatus: 1},
		{name: "scheduled session cut short keeps earlier outfile", mode: ScheduledMode,
			outcomes: []handlers.SessionOutcome{cutShort}, existing: true, wantStatus: 1},
		{name: "scheduled failed read", mode: ScheduledMode, outcomes: []handlers.SessionOutcome{failedRead},
			wantStatus: 1},
		{name: "scheduled failed read on second server", mode: ScheduledMode,
			outcomes: []handlers.SessionOutcome{completed, failedRead}, wantStatus: 1},
		{name: "scheduled without servers", mode: ScheduledMode, outcomes: []handlers.SessionOutcome{},
			wantStatus: 1},
		{name: "scheduled handler without outcome", mode: ScheduledMode, noOutcome: true, wantStatus: 1},
		{name: "scheduled unreadable files", mode: ScheduledMode, outcomes: []handlers.SessionOutcome{unreadable},
			wantStatus: 1},
		{name: "scheduled failed read keeps earlier outfile", mode: ScheduledMode,
			outcomes: []handlers.SessionOutcome{failedRead}, existing: true, wantStatus: 1},

		// After the job's TimeRange ended: file read failures write the
		// partial result, everything else still writes nothing.
		{name: "partial completed", mode: ScheduledPartialMode, wantResult: true},
		{name: "partial failed read", mode: ScheduledPartialMode, outcomes: []handlers.SessionOutcome{failedRead},
			wantResult: true, wantLog: "Writing partial mapreduce result after the job's TimeRange ended" +
				"|srv1: " + protocol.CommandFailureNoFile},
		{name: "partial unreadable files", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{unreadable}, wantResult: true},
		{name: "partial failed read on second server", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{completed, failedRead}, wantResult: true,
			wantLog: "Writing partial mapreduce result after the job's TimeRange ended|srv2: " +
				protocol.CommandFailureNoFile},
		{name: "partial failed reads on both servers", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{failedRead, denied}, wantResult: true,
			wantLog: "Writing partial mapreduce result after the job's TimeRange ended|srv1: " +
				protocol.CommandFailureNoFile + "; srv2: " + protocol.CommandFailurePermission},
		{name: "partial failed read replaces earlier outfile", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{failedRead}, existing: true, wantResult: true},
		{name: "partial failed connection", mode: ScheduledPartialMode, status: 1, wantStatus: 1},
		{name: "partial failed connection and failed read", mode: ScheduledPartialMode, status: 1,
			outcomes: []handlers.SessionOutcome{failedRead}, wantStatus: 1},
		{name: "partial canceled", mode: ScheduledPartialMode, ctxErr: context.Canceled,
			outcomes: []handlers.SessionOutcome{failedRead}, wantStatus: 1},
		{name: "partial session cut short", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{cutShort}, wantStatus: 1},
		{name: "partial failed read on a session cut short", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{failedReadCutShort}, wantStatus: 1},
		{name: "partial failed read and a server cut short", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{failedRead, cutShort}, existing: true, wantStatus: 1},
		{name: "partial invalid query", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{failedQuery}, wantStatus: 1},
		{name: "partial failed read and rejected command", mode: ScheduledPartialMode,
			outcomes: []handlers.SessionOutcome{failedReadAndQuery}, wantStatus: 1},
		{name: "partial without servers", mode: ScheduledPartialMode, outcomes: []handlers.SessionOutcome{},
			wantStatus: 1},
		{name: "partial write error", mode: ScheduledPartialMode, outcomes: []handlers.SessionOutcome{failedRead},
			badDir: true, wantStatus: 1},
		{name: "cumulative session cut short still writes", mode: CumulativeMode,
			outcomes: []handlers.SessionOutcome{cutShort}, wantResult: true},
		{name: "cumulative failed read still writes", mode: CumulativeMode,
			outcomes: []handlers.SessionOutcome{failedRead}, wantResult: true},
		{name: "cumulative failed still writes", mode: CumulativeMode, status: 1, wantStatus: 1, wantResult: true},
		{name: "cumulative canceled still writes", mode: CumulativeMode, ctxErr: context.Canceled, wantResult: true},
		{name: "cumulative write error keeps status", mode: CumulativeMode, badDir: true},
		{name: "default map with outfile failed still writes", mode: DefaultMode, status: 1, wantStatus: 1,
			wantResult: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.badDir {
				dir = filepath.Join(dir, "missing")
			}
			outfile := filepath.Join(dir, "result.csv")
			if tt.existing {
				writeScheduledTestFile(t, outfile, previous)
				writeScheduledTestFile(t, outfile+".query", previous)
			}
			query := outfileTestQuery(t, outfile, false)
			client := newOutfileTestClient(query, tt.mode)
			logger := &warnRecorder{}
			client.loggers.Client = logger
			mergeOutfileTestRow(t, client.session.Snapshot(), 7)
			outcomes := tt.outcomes
			if outcomes == nil {
				outcomes = []handlers.SessionOutcome{completed}
			}
			for i, outcome := range outcomes {
				var handler handlers.Handler = &statusTestHandler{outcome: outcome}
				if tt.noOutcome {
					handler = &retryTestHandler{}
				}
				client.connections = append(client.connections, &retryTestConnector{
					server: fmt.Sprintf("srv%d", i+1), handler: handler})
			}

			if got := client.finish(tt.ctxErr, tt.status); got != tt.wantStatus {
				t.Fatalf("finish() status = %d, want %d", got, tt.wantStatus)
			}

			wantOutfile, wantQuery := "", ""
			switch {
			case tt.wantResult:
				wantOutfile, wantQuery = "count(foo)\n7\n", query.RawQuery
			case tt.existing:
				wantOutfile, wantQuery = previous, previous
			}
			assertScheduledTestFile(t, outfile, wantOutfile)
			assertScheduledTestFile(t, outfile+".query", wantQuery)
			assertScheduledTestFile(t, outfile+".tmp", "")
			assertScheduledTestFile(t, outfile+".query.tmp", "")
			if tt.wantLog != "" && !slices.Contains(logger.warnings(), tt.wantLog) {
				t.Fatalf("warnings = %q, want %q", logger.warnings(), tt.wantLog)
			}
		})
	}
}

// TestMaprClientScheduledStartWritesNoInterimResult runs a scheduled client
// whose only connection aggregates a row and stays open well past the first
// interim report, then ends with the given status. No outfile may appear
// while it runs, and afterwards only a completed query leaves one.
func TestMaprClientScheduledStartWritesNoInterimResult(t *testing.T) {
	t.Parallel()

	for _, status := range []int{0, 1} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			t.Parallel()
			outfile := filepath.Join(t.TempDir(), "result.csv")
			// The first interim report is due after half the 1s interval.
			query := mustMaprClientQuery(t, fmt.Sprintf("from STATS select count(foo) interval 1 outfile %q", outfile))
			client := newOutfileTestClient(query, ScheduledMode)
			conn := &scheduledTestConnector{
				handler: &statusTestHandler{status: status, outcome: handlers.SessionOutcome{Completed: true}},
				run: func() {
					mergeOutfileTestRow(t, client.session.Snapshot(), 7)
					time.Sleep(1500 * time.Millisecond)
				},
				sawOutfile: func() bool {
					_, err := os.Stat(outfile)
					return err == nil
				},
			}
			client.mu = newBaseClientMu()
			client.stats = newTailStats(1, nil, 0, nil)
			client.connections = []connectors.Connector{conn}

			if got := client.Start(context.Background(), make(chan string)); got != status {
				t.Fatalf("Start() status = %d, want %d", got, status)
			}
			if conn.outfileDuringRun {
				t.Fatal("scheduled client wrote an interim outfile while the query was running")
			}
			want := ""
			if status == 0 {
				want = "count(foo)\n7\n"
			}
			assertScheduledTestFile(t, outfile, want)
		})
	}
}

// scheduledTestConnector runs run as its connection and records whether the
// outfile existed before the connection ended.
type scheduledTestConnector struct {
	retryTestConnector
	handler          handlers.Handler
	run              func()
	sawOutfile       func() bool
	outfileDuringRun bool
}

func (c *scheduledTestConnector) Start(context.Context, context.CancelFunc, chan struct{}, chan struct{}) {
	c.run()
	c.outfileDuringRun = c.sawOutfile()
}

func (c *scheduledTestConnector) Handler() handlers.Handler { return c.handler }

// statusTestHandler is a handler whose connection ended with status and
// outcome.
type statusTestHandler struct {
	retryTestHandler
	status  int
	outcome handlers.SessionOutcome
}

func (h *statusTestHandler) Status() int { return h.status }

func (h *statusTestHandler) Outcome() handlers.SessionOutcome { return h.outcome }

// TestSessionOutcome covers when a scheduled query takes a server's session
// as complete, and which failures it may write a partial result for.
func TestSessionOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		handler      handlers.Handler
		complete     bool
		fileFailures []string
	}{
		{name: "closed by the server", handler: &statusTestHandler{outcome: handlers.SessionOutcome{Completed: true}},
			complete: true},
		{name: "cut short", handler: &statusTestHandler{}},
		{name: "file read failures", handler: &statusTestHandler{outcome: handlers.SessionOutcome{Completed: true,
			Failures: []string{protocol.CommandFailureNoFile, protocol.CommandFailurePermission}}},
			complete: true, fileFailures: []string{"srv1: " + protocol.CommandFailureNoFile,
				"srv1: " + protocol.CommandFailurePermission}},
		{name: "unparsable read command", handler: &statusTestHandler{outcome: handlers.SessionOutcome{
			Completed: true, Failures: []string{protocol.CommandFailureReadCommand}}}},
		{name: "unknown failure", handler: &statusTestHandler{outcome: handlers.SessionOutcome{
			Completed: true, Failures: []string{"read: something new"}}}},
		{name: "file read failure and cut short", handler: &statusTestHandler{
			outcome: handlers.SessionOutcome{Failures: []string{protocol.CommandFailureNoFile}}}},
		{name: "no outcome", handler: &retryTestHandler{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			incomplete, fileFailures := sessionOutcome("srv1", tt.handler)
			if (incomplete == "") != tt.complete {
				t.Fatalf("sessionOutcome() incomplete = %q, want complete=%v", incomplete, tt.complete)
			}
			if !slices.Equal(fileFailures, tt.fileFailures) {
				t.Fatalf("sessionOutcome() file failures = %q, want %q", fileFailures, tt.fileFailures)
			}
		})
	}
}

// warnRecorder records the client's warnings, each with its arguments joined
// with "|".
type warnRecorder struct {
	clientlog.NopLogger
	mu    sync.Mutex
	lines []string
}

func (l *warnRecorder) Warn(args ...any) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = fmt.Sprint(arg)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.Join(parts, "|"))
	return ""
}

func (l *warnRecorder) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

func writeScheduledTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

// assertScheduledTestFile checks path holds want, or does not exist when want
// is empty.
func assertScheduledTestFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want == "" {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists (content %q, err %v), want no file", path, got, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
