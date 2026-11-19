package client

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"

	"golang.org/x/crypto/ssh/knownhosts"
)

type recordingLogger struct {
	logging.NopLogger
	debugCount     atomic.Int32
	infoCount      atomic.Int32
	warnCount      atomic.Int32
	errorCount     atomic.Int32
	paused         atomic.Bool
	logWhilePaused atomic.Bool
	debugCh        chan struct{}
	mutex          sync.Mutex
	lastError      error
	lastWarn       error
}

func (l *recordingLogger) Debug(...any) string {
	l.debugCount.Add(1)
	if l.debugCh != nil {
		select {
		case l.debugCh <- struct{}{}:
		default:
		}
	}
	if l.paused.Load() {
		l.logWhilePaused.Store(true)
	}
	return ""
}

func (l *recordingLogger) Info(...any) string {
	l.infoCount.Add(1)
	if l.paused.Load() {
		l.logWhilePaused.Store(true)
	}
	return ""
}

func (l *recordingLogger) Warn(args ...any) string {
	l.warnCount.Add(1)
	if l.paused.Load() {
		l.logWhilePaused.Store(true)
	}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			l.mutex.Lock()
			l.lastWarn = err
			l.mutex.Unlock()
		}
	}
	return ""
}

func (l *recordingLogger) lastWarnValue() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.lastWarn
}

func (l *recordingLogger) Error(args ...any) string {
	l.errorCount.Add(1)
	if l.paused.Load() {
		l.logWhilePaused.Store(true)
	}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			l.mutex.Lock()
			l.lastError = err
			l.mutex.Unlock()
		}
	}
	return ""
}

func (l *recordingLogger) Pause() {
	l.paused.Store(true)
}

func (l *recordingLogger) Resume() {
	l.paused.Store(false)
}

func (l *recordingLogger) lastErrorValue() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.lastError
}

func TestPromptAddHostsLogsAfterResume(t *testing.T) {
	tests := []struct {
		name         string
		answer       string
		wantTrustAll bool
	}{
		{name: "trust batch", answer: "yes\n"},
		{name: "trust all", answer: "all\n", wantTrustAll: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
			if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}

			logger := &recordingLogger{}
			callback := testKnownHostsCallbackWithLogger(t, knownHostsPath, logger)
			withPromptInput(t, test.answer, func() {
				callback.promptAddHosts([]unknownHost{testUnknownHost("new.example:2222", "new")})
			})

			if got := logger.infoCount.Load(); got != 1 {
				t.Fatalf("Info calls = %d, want 1", got)
			}
			if got := logger.warnCount.Load(); got != 0 {
				t.Fatalf("Warn calls = %d, want 0 for a locked update", got)
			}
			if logger.logWhilePaused.Load() {
				t.Fatal("known-host callback logged before the prompt resumed logging")
			}
			if logger.paused.Load() {
				t.Fatal("prompt left logging paused")
			}
			select {
			case <-callback.trustAllHostsCh:
				if !test.wantTrustAll {
					t.Fatal("trust-all state set for a single-batch answer")
				}
			default:
				if test.wantTrustAll {
					t.Fatal("trust-all state not set for an all answer")
				}
			}
		})
	}
}

func TestPromptAddHostsFailureLogsAfterResume(t *testing.T) {
	tmp := t.TempDir()
	knownHostsPath := filepath.Join(tmp, "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	logger := &recordingLogger{}
	callback := testKnownHostsCallbackWithLogger(t, knownHostsPath, logger)
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatalf("remove known-hosts directory: %v", err)
	}
	withPromptInput(t, "yes\n", func() {
		callback.promptAddHosts([]unknownHost{testUnknownHost("new.example:2222", "new")})
	})

	if got := logger.errorCount.Load(); got != 1 {
		t.Fatalf("Error calls = %d, want 1", got)
	}
	if logger.logWhilePaused.Load() {
		t.Fatal("known-host failure logged before the prompt resumed logging")
	}
	if logger.paused.Load() {
		t.Fatal("prompt left logging paused after trust failure")
	}
}

func TestPromptAddHostsCleanupFailureIsReportedAfterResume(t *testing.T) {
	tmp := t.TempDir()
	knownHostsPath := filepath.Join(tmp, "known_hosts")
	if err := os.Mkdir(knownHostsPath, 0o700); err != nil {
		t.Fatalf("create invalid known-hosts directory: %v", err)
	}

	logger := &recordingLogger{}
	callback := testKnownHostsCallbackWithLogger(t, knownHostsPath, logger)
	cleanupErr := errors.New("forced cleanup failure")
	callback.removeTempFile = func(*os.Root, string) error { return cleanupErr }
	withPromptInput(t, "yes\n", func() {
		callback.promptAddHosts([]unknownHost{testUnknownHost("new.example:2222", "new")})
	})

	if got := logger.debugCount.Load(); got != 0 {
		t.Fatalf("Debug calls = %d, want 0 from the paused callback", got)
	}
	if got := logger.errorCount.Load(); got != 1 {
		t.Fatalf("Error calls = %d, want 1 after Resume", got)
	}
	if logger.logWhilePaused.Load() {
		t.Fatal("temporary-file cleanup logged before the prompt resumed logging")
	}
	loggedErr := logger.lastErrorValue()
	if !errors.Is(loggedErr, cleanupErr) {
		t.Fatalf("logged error %v does not retain cleanup error %v", loggedErr, cleanupErr)
	}
	if !strings.Contains(loggedErr.Error(), "open known hosts file") {
		t.Fatalf("logged error does not retain primary open failure: %v", loggedErr)
	}
}

func TestTrustHostsAppendsDistinctExistingEntries(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	existingLine := knownhosts.Line([]string{"old.example:2222"}, &mockPublicKey{id: "old"})
	if err := os.WriteFile(knownHostsPath, []byte(existingLine+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	callback := testKnownHostsCallback(t, knownHostsPath)
	unknown := testUnknownHost("new.example:2222", "new")

	if _, err := callback.trustHosts([]unknownHost{unknown}); err != nil {
		t.Fatalf("trustHosts failed: %v", err)
	}

	got, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	want := strings.Join([]string{
		unknown.hostLine,
		unknown.ipLine,
		existingLine,
		"",
	}, "\n")
	if string(got) != want {
		t.Fatalf("trustHosts wrote:\n%s\nwant:\n%s", got, want)
	}

	if trustResponse := <-unknown.responseCh; trustResponse != trustHost {
		t.Fatalf("unexpected trust response: %v", trustResponse)
	}
}

func TestTrustHostsReplacesExistingEntriesForSameHostAndIP(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	oldUnknown := testUnknownHost("replace.example:2222", "old")
	keepLine := knownhosts.Line([]string{"keep.example:2222"}, &mockPublicKey{id: "keep"})
	initialContents := strings.Join([]string{
		oldUnknown.hostLine,
		oldUnknown.ipLine,
		keepLine,
		"",
	}, "\n")
	if err := os.WriteFile(knownHostsPath, []byte(initialContents), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	callback := testKnownHostsCallback(t, knownHostsPath)
	newUnknown := testUnknownHost("replace.example:2222", "new")

	if _, err := callback.trustHosts([]unknownHost{newUnknown}); err != nil {
		t.Fatalf("trustHosts failed: %v", err)
	}

	got, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	want := strings.Join([]string{
		newUnknown.hostLine,
		newUnknown.ipLine,
		keepLine,
		"",
	}, "\n")
	if string(got) != want {
		t.Fatalf("trustHosts wrote:\n%s\nwant:\n%s", got, want)
	}

	if trustResponse := <-newUnknown.responseCh; trustResponse != trustHost {
		t.Fatalf("unexpected trust response: %v", trustResponse)
	}
}

func TestTrustHostsRejectsEscapingKnownHostsSymlink(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "ssh")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	outsidePath := filepath.Join(filepath.Dir(rootDir), "outside_known_hosts")
	if err := os.WriteFile(outsidePath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	knownHostsPath := filepath.Join(rootDir, "known_hosts")
	if err := os.Symlink(filepath.Join("..", "outside_known_hosts"), knownHostsPath); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	callback := testKnownHostsCallback(t, knownHostsPath)
	unknown := testUnknownHost("escape.example:2222", "new")

	if _, err := callback.trustHosts([]unknownHost{unknown}); err == nil {
		t.Fatalf("trustHosts succeeded for escaping known_hosts symlink")
	}
}

// TestWrapReturnsWhenCtxCancelledBeforeUnknownChSend verifies that when
// PromptAddHosts has already exited (no consumer on unknownCh), the wrapped
// host-key callback unblocks on ctx cancel instead of hanging the SSH
// handshake and leaking a goroutine. Pre-fix this test times out because
// `c.unknownCh <- unknown` blocks forever.
func TestWrapReturnsWhenCtxCancelledBeforeUnknownChSend(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	logger := &recordingLogger{debugCh: make(chan struct{}, 1)}
	callback := testKnownHostsCallbackWithLogger(t, knownHostsPath, logger)
	ctx, cancel := context.WithCancel(context.Background())

	wrapped := callback.Wrap(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- wrapped("host.example:2222", testTCPAddr("127.0.0.1:2222"),
			&mockPublicKey{id: "new"})
	}()

	select {
	case <-logger.debugCh:
	case <-time.After(time.Second):
		t.Fatal("host key callback did not reach unknown-host notification")
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected non-nil error after ctx cancel, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected error to wrap context.Canceled, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("host key callback did not return within 100ms after ctx cancel")
	}
	if logger.debugCount.Load() == 0 {
		t.Fatal("injected logger did not receive unknown-host diagnostic")
	}
}

// TestWrapReturnsWhenCtxCancelledBeforeResponse verifies that when a consumer
// has picked the unknown host off unknownCh but never writes a response
// (e.g. PromptAddHosts was cancelled mid-batch), the callback still unblocks
// on ctx cancel rather than blocking on responseCh forever.
func TestWrapReturnsWhenCtxCancelledBeforeResponse(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	callback := testKnownHostsCallback(t, knownHostsPath)
	ctx, cancel := context.WithCancel(context.Background())

	// Simulate a consumer that drains unknownCh but never writes to
	// responseCh, mimicking PromptAddHosts buffering a batch and then exiting.
	consumed := make(chan struct{})
	go func() {
		<-callback.unknownCh
		close(consumed)
	}()

	wrapped := callback.Wrap(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- wrapped("host.example:2222", testTCPAddr("127.0.0.1:2222"),
			&mockPublicKey{id: "new"})
	}()

	select {
	case <-consumed:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("consumer never received unknown host")
	}

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected non-nil error after ctx cancel, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected error to wrap context.Canceled, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("host key callback did not return within 100ms after ctx cancel")
	}
}

// TestCloseTrustAllHostsChConcurrentNoPanic verifies that concurrent calls to
// closeTrustAllHostsCh — the method that replaces the racy select/default/close
// pattern — never panic and leave the channel durably closed. With -race this
// also detects any data race on the underlying sync.Once / trustAllHostsCh.
//
// Pre-fix, the equivalent inline select/default/close code in the "all" answer
// callback was not atomic: two goroutines could both observe the channel open,
// both take the default branch, and both call close() — causing a panic.
func TestCloseTrustAllHostsChConcurrentNoPanic(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	callback := testKnownHostsCallback(t, knownHostsPath)

	// Run many goroutines concurrently to maximise the chance of hitting the
	// race window that existed before the sync.Once fix.
	const concurrency = 50
	ready := make(chan struct{})
	done := make(chan struct{}, concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			<-ready
			// closeTrustAllHostsCh is the idempotent replacement for the racy
			// select/default/close sequence. All calls must be safe.
			callback.closeTrustAllHostsCh()
			done <- struct{}{}
		}()
	}

	// Release all goroutines simultaneously to maximise contention.
	close(ready)
	for i := 0; i < concurrency; i++ {
		<-done
	}

	// The channel must be closed exactly once: a receive on a closed channel
	// returns immediately with the zero value.
	select {
	case <-callback.trustAllHostsCh:
		// OK – closed by exactly one goroutine via sync.Once.
	default:
		t.Fatalf("trustAllHostsCh is still open after concurrent closeTrustAllHostsCh calls")
	}
}

func testKnownHostsCallback(t *testing.T, knownHostsPath string) *KnownHostsCallback {
	t.Helper()
	return testKnownHostsCallbackWithLogger(t, knownHostsPath, sshClientTestLogger)
}

func testKnownHostsCallbackWithLogger(t *testing.T, knownHostsPath string,
	logger logging.Logger) *KnownHostsCallback {
	t.Helper()

	callback, err := NewKnownHostsCallback(knownHostsPath, false, logger, logger)
	if err != nil {
		t.Fatalf("NewKnownHostsCallback failed: %v", err)
	}

	knownHostsCallback, ok := callback.(*KnownHostsCallback)
	if !ok {
		t.Fatalf("unexpected callback type %T", callback)
	}

	return knownHostsCallback
}

func testUnknownHost(server, keyID string) unknownHost {
	key := &mockPublicKey{id: keyID}
	remote := testTCPAddr("127.0.0.1:2222")

	return unknownHost{
		server:     server,
		remote:     remote,
		key:        key,
		hostLine:   knownhosts.Line([]string{server}, key),
		ipLine:     knownhosts.Line([]string{remote.String()}, key),
		responseCh: make(chan response, 1),
	}
}

func testTCPAddr(address string) *net.TCPAddr {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		panic(err)
	}

	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		panic(err)
	}

	return &net.TCPAddr{IP: net.ParseIP(host), Port: port}
}

func withPromptInput(t *testing.T, contents string, run func()) {
	t.Helper()
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create prompt input: %v", err)
	}
	previousStdin := os.Stdin
	os.Stdin = input
	defer func() {
		os.Stdin = previousStdin
		_ = input.Close()
		_ = inputWriter.Close()
	}()

	if _, err := inputWriter.WriteString(contents); err != nil {
		t.Fatalf("write prompt input: %v", err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close prompt input: %v", err)
	}
	run()
}
