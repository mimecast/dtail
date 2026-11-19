package handlers

// Tests of what a probe logs: the probe and the path listing check many
// paths every round of every interactive session, so their permission
// checks log per path at debug level only, and the probe logs one summary
// line ("Probe answered") at info level instead.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// levelCountingLogger records the messages logged at error, warn and info
// level; it is safe for concurrent use, as a probe runs on a goroutine of
// its own.
type levelCountingLogger struct {
	logging.NopLogger
	mu       sync.Mutex
	messages []string
}

func (l *levelCountingLogger) record(level string, args ...any) string {
	message := level + " " + fmt.Sprint(args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, message)
	return message
}

func (l *levelCountingLogger) Error(args ...any) string { return l.record("ERROR", args...) }
func (l *levelCountingLogger) Warn(args ...any) string  { return l.record("WARN", args...) }
func (l *levelCountingLogger) Info(args ...any) string  { return l.record("INFO", args...) }

// take returns the recorded messages and forgets them.
func (l *levelCountingLogger) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	messages := l.messages
	l.messages = nil
	return messages
}

// newProbeLoggingHandler returns a ServerHandler of a user with permissions
// whose user and handler log into logger.
func newProbeLoggingHandler(t *testing.T, permissions []string, logger logging.Logger) *ServerHandler {
	t.Helper()
	lookup := func(string) ([]string, error) { return permissions, nil }
	user, err := userserver.New("probe-user", "127.0.0.1", lookup, logger)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	handler, err := NewServerHandler(context.Background(), user, Dependencies{
		CatLimiter:   make(chan struct{}, 4),
		TailLimiter:  make(chan struct{}, 4),
		ServerConfig: &config.ServerConfig{MapreduceLogFormat: "default"},
		AuthKeyStore: authkey.New(time.Hour, 5),
		Loggers:      HandlerLoggers{Diagnostics: logger, Reader: logger},
		Hostname:     "probe-host.example.org",
	})
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	return handler
}

// requireOnlyProbeSummary fails unless messages is the one "Probe answered"
// summary line of a probe.
func requireOnlyProbeSummary(t *testing.T, what string, messages []string) {
	t.Helper()
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "INFO ") ||
		!strings.Contains(messages[0], "Probe answered") {
		shown := messages
		if len(shown) > 5 {
			shown = shown[:5]
		}
		t.Fatalf("%s logged %d lines at info level or above, want the summary only; first: %q",
			what, len(messages), shown)
	}
}

// TestProbeListLogsSummaryOnly: listing a directory of 400 permitted and
// 100 denied files logs one info line, not one or two per file.
func TestProbeListLogsSummaryOnly(t *testing.T) {
	dir := realTempDir(t)
	logger := &levelCountingLogger{}
	quoted := regexp.QuoteMeta(dir)
	handler := newProbeLoggingHandler(t, []string{
		"readfiles:^" + quoted + "/", "readfiles:!^" + quoted + "/denied",
	}, logger)
	for i := range 400 {
		writeProbeTestFile(t, dir, fmt.Sprintf("app%03d.log", i), "line")
	}
	for i := range 100 {
		writeProbeTestFile(t, dir, fmt.Sprintf("denied%03d.log", i), "line")
	}
	logger.take() // the user's and handler's construction

	answer := probeList(t, handler, dir+"/")
	if len(answer.Entries) != 400 || answer.Truncated {
		t.Fatalf("entries = %d, truncated = %v, want 400 complete", len(answer.Entries), answer.Truncated)
	}
	requireOnlyProbeSummary(t, "listing", logger.take())
}

// TestProbeLogsFailedEntries: a field probe logs one warning per request
// entry and reason it could not read, naming the entry, the reason and the
// number of matches, not one line per match: a glob matching 300 denied
// and 5 permitted files, a file a deny rule denies, a file no allow rule
// covers and a glob matching nothing give one warning each, plus the
// "Probe answered" summary. A probe is thus no silent existence oracle,
// and its log stays bounded by the request's entries.
func TestProbeLogsFailedEntries(t *testing.T) {
	dir := realTempDir(t)
	logger := &levelCountingLogger{}
	quoted := regexp.QuoteMeta(dir)
	handler := newProbeLoggingHandler(t, []string{
		"readfiles:^" + quoted + "/ok", "readfiles:!^" + quoted + "/ok-secret",
	}, logger)
	for i := range 300 {
		writeProbeTestFile(t, dir, fmt.Sprintf("denied%03d.log", i), "line")
	}
	for i := range 5 {
		writeProbeTestFile(t, dir, fmt.Sprintf("ok%d.log", i), probeTestStatsLine("x", "1"))
	}
	secret := writeProbeTestFile(t, dir, "ok-secret.log", "line")
	logger.take()

	glob, uncovered, missing := dir+"/*.log", dir+"/denied000.log", dir+"/none*.log"
	answer, _ := probeFiles(t, handler, glob, secret, uncovered, missing)
	if answer.FilesSampled != 5 || !hasProbeError(answer, glob, protocol.CommandFailurePermission) {
		t.Fatalf("answer = %+v, want 5 files sampled and the permission error", answer)
	}
	messages := logger.take()
	want := map[string]string{
		glob:      protocol.CommandFailurePermission + "matches301",
		secret:    protocol.CommandFailurePermission + "matches1",
		uncovered: protocol.CommandFailurePermission + "matches1",
		missing:   protocol.CommandFailureNoFile + "matches0",
	}
	if len(messages) != len(want)+1 {
		t.Fatalf("logged %d lines at info level or above, want %d: %q", len(messages), len(want)+1, messages)
	}
	requireOnlyProbeSummary(t, "probe", messages[len(messages)-1:])
	for entry, tail := range want {
		if !slices.ContainsFunc(messages, func(m string) bool {
			return strings.HasPrefix(m, "WARN ") && strings.HasSuffix(m, "Probe: unable to read entry"+entry+tail)
		}) {
			t.Errorf("no warning for %s ending in %q: %q", entry, tail, messages)
		}
	}
}

// TestReadPermissionCheckStillLogs: the permission check of a read command
// (PrepareReadTarget) keeps its documented info lines, unlike the probe's.
func TestReadPermissionCheckStillLogs(t *testing.T) {
	dir := realTempDir(t)
	logger := &levelCountingLogger{}
	handler := newProbeLoggingHandler(t, []string{"readfiles:^" + regexp.QuoteMeta(dir) + "/"}, logger)
	path := writeProbeTestFile(t, dir, "app.log", "line")
	logger.take()

	if _, err := handler.PrepareReadTarget(path); err != nil {
		t.Fatalf("PrepareReadTarget: %v", err)
	}
	messages := strings.Join(logger.take(), "\n")
	for _, want := range []string{"User with OS file system permissions to path",
		"Permission test passed partially"} {
		if !strings.Contains(messages, "INFO ") || !strings.Contains(messages, want) {
			t.Fatalf("read permission check logged %q, want an info line %q", messages, want)
		}
	}
}

// BenchmarkProbeList500 lists a directory of 500 permitted files through a
// session's probe with one and with five rules, and reports the lines
// logged at info level or above per listing.
func BenchmarkProbeList500(b *testing.B) {
	for _, rules := range []int{1, 5} {
		b.Run(fmt.Sprintf("rules=%d", rules), func(b *testing.B) {
			dir, err := filepath.EvalSymlinks(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			quoted := regexp.QuoteMeta(dir)
			permissions := []string{"readfiles:^" + quoted + "/"}
			for i := 1; i < rules; i++ {
				permissions = append(permissions, fmt.Sprintf("readfiles:!^%s/secret%d/.*\\.log$", quoted, i))
			}
			for i := range 500 {
				path := filepath.Join(dir, fmt.Sprintf("app%03d.log", i))
				if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			benchmarkProbeList(b, dir, permissions)
		})
	}
}

func benchmarkProbeList(b *testing.B, dir string, permissions []string) {
	logger := &levelCountingLogger{}
	lookup := func(string) ([]string, error) { return permissions, nil }
	user, err := userserver.New("probe-user", "127.0.0.1", lookup, logger)
	if err != nil {
		b.Fatal(err)
	}
	handler, err := NewServerHandler(context.Background(), user, Dependencies{
		CatLimiter:   make(chan struct{}, 4),
		TailLimiter:  make(chan struct{}, 4),
		ServerConfig: &config.ServerConfig{MapreduceLogFormat: "default"},
		AuthKeyStore: authkey.New(time.Hour, 5),
		Loggers:      HandlerLoggers{Diagnostics: logger, Reader: logger},
	})
	if err != nil {
		b.Fatal(err)
	}
	p := handler.newProber()
	request := protocol.ProbeRequest{ID: "l1", List: dir + "/"}
	logger.take()
	b.ReportAllocs()
	for b.Loop() {
		if answer := p.run(context.Background(), request); len(answer.Entries) != 500 {
			b.Fatalf("entries = %d, want 500", len(answer.Entries))
		}
	}
	b.ReportMetric(float64(len(logger.take()))/float64(b.N), "loglines/op")
}
