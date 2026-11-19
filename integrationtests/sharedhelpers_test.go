package integrationtests

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	osuser "os/user"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func currentUsername(t *testing.T) string {
	t.Helper()

	user, err := osuser.Current()
	if err != nil {
		t.Fatalf("get current user: %v", err)
	}
	return user.Username
}

func readIntegrationPublicKey(t *testing.T) []byte {
	t.Helper()

	path := config.IntegrationSSHPrivateKeyPath() + ".pub"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read integration public key %s: %v", path, err)
	}
	return data
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func permissionForPath(path string) string {
	return "readfiles:^" + regexp.QuoteMeta(path) + "$"
}

type dJournalExtendedServer struct {
	ctx     context.Context
	address string
	logs    *safeLineLog
}

// startDJournalExtendedServer starts a dserver for integration tests that need
// a live server with captured logs. Despite the name it is not journal-specific.
func startDJournalExtendedServer(t *testing.T, logger *TestLogger, configFile string,
	env map[string]string, logLevel string, dropIntegrationEnv bool) dJournalExtendedServer {

	t.Helper()

	port := getUniquePortNumber()
	bindAddress := "localhost"
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTestLogger(ctx, logger)
	logs := &safeLineLog{}
	cmd := exec.CommandContext(ctx, "../dserver",
		"--cfg", configFile,
		"--logger", "stdout",
		"--logLevel", logLevel,
		"--bindAddress", bindAddress,
		"--port", fmt.Sprintf("%d", port),
	)
	cmd.Env = mergedCommandEnv(env, dropIntegrationEnv)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("open dserver stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatalf("open dserver stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start dserver: %v", err)
	}
	go scanToLog(stdout, logs)
	go scanToLog(stderr, logs)

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-errCh
		}
	})

	if err := waitForServerReady(ctx, bindAddress, port); err != nil {
		cancel()
		t.Fatalf("wait for dserver: %v\nlogs:\n%s", err, logs.String())
	}

	return dJournalExtendedServer{
		ctx:     ctx,
		address: fmt.Sprintf("%s:%d", bindAddress, port),
		logs:    logs,
	}
}

type safeLineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *safeLineLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *safeLineLog) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (l *safeLineLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (l *safeLineLog) waitContains(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if l.contains(substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server logs did not contain %q before timeout; logs:\n%s", substr, l.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// count returns how many log lines contain substr.
func (l *safeLineLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func scanToLog(r io.Reader, logs *safeLineLog) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logs.add(scanner.Text())
	}
}

func mergedCommandEnv(overrides map[string]string, dropIntegrationEnv bool) []string {
	skip := make(map[string]struct{}, len(overrides)+2)
	for key := range overrides {
		skip[key] = struct{}{}
	}
	if dropIntegrationEnv {
		skip["DTAIL_INTEGRATION_TEST_RUN_MODE"] = struct{}{}
		skip["DTAIL_TURBOBOOST_DISABLE"] = struct{}{}
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, found := strings.Cut(item, "=")
		if found {
			if _, ok := skip[key]; ok {
				continue
			}
		}
		env = append(env, item)
	}
	for key, value := range overrides {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}
