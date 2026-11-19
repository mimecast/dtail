package sessionuser

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// levelLogger records every message with its level.
type levelLogger struct {
	logging.NopLogger
	mu       sync.Mutex
	messages []string
}

func (l *levelLogger) record(level string, args ...any) string {
	message := level + " " + fmt.Sprint(args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, message)
	return message
}

func (l *levelLogger) Error(args ...any) string { return l.record("ERROR", args...) }
func (l *levelLogger) Warn(args ...any) string  { return l.record("WARN", args...) }
func (l *levelLogger) Info(args ...any) string  { return l.record("INFO", args...) }
func (l *levelLogger) Debug(args ...any) string { return l.record("DEBUG", args...) }

// take returns the recorded messages and forgets them.
func (l *levelLogger) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	messages := l.messages
	l.messages = nil
	return messages
}

// aboveDebug returns the messages logged above debug level.
func aboveDebug(messages []string) []string {
	var above []string
	for _, message := range messages {
		if !strings.HasPrefix(message, "DEBUG ") {
			above = append(above, message)
		}
	}
	return above
}

// TestResolveReadTargetQuietly: the quiet check gives the result of
// ResolveReadTarget for a permitted file, a denied file, a dangling
// symlink and a rule that does not compile, but logs about the path at
// debug level only, where ResolveReadTarget logs at its documented levels.
func TestResolveReadTargetQuietly(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	quoted := regexp.QuoteMeta(dir)
	allowed := filepath.Join(dir, "app.log")
	denied := filepath.Join(dir, "secret.log")
	for _, path := range []string{allowed, denied} {
		if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dangling := filepath.Join(dir, "dangling.log")
	if err := os.Symlink(filepath.Join(dir, "missing.log"), dangling); err != nil {
		t.Fatal(err)
	}
	rules := []string{"readfiles:!^" + quoted + "/secret", "readfiles:^" + quoted + "/"}
	invalid := []string{"readfiles:^" + quoted + "/(", "readfiles:^/"}

	for _, tc := range []struct {
		name, path string
		rules      []string
	}{
		{"permitted", allowed, rules},
		{"denied", denied, rules},
		{"dangling", dangling, rules},
		{"invalid rule", allowed, invalid},
	} {
		logger := &levelLogger{}
		u := newTestUser(tc.rules)
		u.logger = logger
		_, verboseErr := u.ResolveReadTarget(tc.path, "readfiles")
		verbose := aboveDebug(logger.take())
		_, quietErr := u.ResolveReadTargetQuietly(tc.path, "readfiles")
		quiet := logger.take()
		if fmt.Sprint(verboseErr) != fmt.Sprint(quietErr) {
			t.Errorf("%s: quiet error %v, verbose error %v", tc.name, quietErr, verboseErr)
		}
		if len(verbose) == 0 {
			t.Errorf("%s: the verbose check logged nothing above debug level", tc.name)
		}
		if above := aboveDebug(quiet); len(above) != 0 || len(quiet) == 0 {
			t.Errorf("%s: the quiet check logged %q above debug level (%d lines in all)",
				tc.name, above, len(quiet))
		}
	}
}

// TestIteratePathsCompilesOnce: a check matches the rules compiled for the
// user once, so it allocates nothing (compiling a regex per check
// allocated for every rule and path).
func TestIteratePathsCompilesOnce(t *testing.T) {
	u := newTestUser([]string{
		"readfiles:!^/var/log/secret/", "readfiles:^/var/log/", "readfiles:!^/var/log/private/.*\\.log$",
	})
	if ok, err := u.iteratePaths("/var/log/app.log", "readfiles", u.checkLog(true)); !ok || err != nil {
		t.Fatalf("iteratePaths = %v, %v, want permitted", ok, err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = u.iteratePaths("/var/log/app.log", "readfiles", u.checkLog(true))
	})
	if allocs != 0 {
		t.Fatalf("iteratePaths allocated %.0f times per check, want 0", allocs)
	}
}
