package connectors

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"

	"golang.org/x/crypto/ssh"
)

// TestResolveAuthKeyPathNoLiteralPath is a regression test for the bug
// described in task l6: when authKeyPath is empty and HOME is also unset,
// resolveAuthKeyPath must return "" instead of a mangled path like
// "/.ssh/id_rsa" or the literal "~/.ssh/id_rsa" that the SSH stack cannot use.
func TestResolveAuthKeyPathNoLiteralPath(t *testing.T) {
	// Unset HOME so the environment fallback is also empty.
	t.Setenv("HOME", "")

	got := resolveAuthKeyPath("")
	if got != "" {
		t.Fatalf("resolveAuthKeyPath(\"\") with empty HOME = %q; want \"\"", got)
	}
}

// TestResolveAuthKeyPathExplicitPathPassedThrough verifies that a non-empty
// explicit auth key path is returned unchanged.
func TestResolveAuthKeyPathExplicitPathPassedThrough(t *testing.T) {
	got := resolveAuthKeyPath("/custom/key")
	if got != "/custom/key" {
		t.Fatalf("resolveAuthKeyPath(\"/custom/key\") = %q; want \"/custom/key\"", got)
	}
}

// TestResolveAuthKeyPathFallsBackToHome verifies that when authKeyPath is empty
// but HOME is set, the function returns the expected default path.
func TestResolveAuthKeyPathFallsBackToHome(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")

	got := resolveAuthKeyPath("")
	want := "/home/testuser/.ssh/id_rsa"
	if got != want {
		t.Fatalf("resolveAuthKeyPath(\"\") = %q; want %q", got, want)
	}
}

func TestExtractAuthKeyBase64(t *testing.T) {
	t.Run("valid authorized key line", func(t *testing.T) {
		pubKey := []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA user@host\n")

		got, err := extractAuthKeyBase64(pubKey)
		if err != nil {
			t.Fatalf("Expected valid key, got error: %v", err)
		}
		if got != "AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
			t.Fatalf("Unexpected base64 payload: %s", got)
		}
	})

	t.Run("invalid key format", func(t *testing.T) {
		_, err := extractAuthKeyBase64([]byte("not-a-valid-authorized-key-line"))
		if err == nil {
			t.Fatalf("Expected parse error for invalid key format")
		}
	})

	t.Run("invalid base64 payload", func(t *testing.T) {
		_, err := extractAuthKeyBase64([]byte("ssh-ed25519 !!! not-valid\n"))
		if err == nil {
			t.Fatalf("Expected error for invalid base64 payload")
		}
	})
}

func TestSendAuthKeyRegistrationCommand(t *testing.T) {
	tempDir := t.TempDir()
	privateKeyPath := filepath.Join(tempDir, "id_rsa")
	publicKeyPath := privateKeyPath + ".pub"
	if err := os.WriteFile(publicKeyPath,
		[]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA user@host\n"), 0600); err != nil {
		t.Fatalf("Unable to write public key test file: %v", err)
	}

	handler := &mockHandler{}
	conn := &ServerConnection{
		server:      "srv1",
		handler:     handler,
		authKeyPath: privateKeyPath,
	}

	conn.sendAuthKeyRegistrationCommand()

	if len(handler.commands) != 1 {
		t.Fatalf("Expected one AUTHKEY command, got %d", len(handler.commands))
	}
	expected := "AUTHKEY AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if handler.commands[0] != expected {
		t.Fatalf("Unexpected AUTHKEY command.\nexpected: %s\ngot:      %s", expected, handler.commands[0])
	}
}

func TestNewServerConnectionUsesInjectedSettings(t *testing.T) {

	conn, err := NewServerConnection(
		"srv1",
		"user",
		nil,
		testHostKeyCallback{},
		&mockHandler{},
		nil,
		sessionspec.Spec{},
		false,
		"",
		false,
		testSSHSettings{port: 3022, timeout: 5 * time.Second},
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("NewServerConnection: %v", err)
	}

	if conn.hostname != "srv1" {
		t.Fatalf("Expected hostname srv1, got %q", conn.hostname)
	}
	if conn.port != 3022 {
		t.Fatalf("Expected injected port 3022, got %d", conn.port)
	}
	if conn.config.Timeout != 5*time.Second {
		t.Fatalf("Expected injected timeout 5s, got %v", conn.config.Timeout)
	}
}

func TestNewServerConnectionFallsBackToDefaults(t *testing.T) {

	conn, err := NewServerConnection(
		"srv1",
		"user",
		nil,
		testHostKeyCallback{},
		&mockHandler{},
		nil,
		sessionspec.Spec{},
		false,
		"",
		false,
		testSSHSettings{},
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("NewServerConnection: %v", err)
	}

	if conn.port != defaultSSHPort {
		t.Fatalf("Expected default port %d, got %d", defaultSSHPort, conn.port)
	}
	if conn.config.Timeout != defaultSSHConnectTimeout {
		t.Fatalf("Expected default timeout %v, got %v", defaultSSHConnectTimeout, conn.config.Timeout)
	}
}

func TestNewServerConnectionReturnsInvalidPortError(t *testing.T) {

	tests := []string{
		"",
		" ",
		":2222",
		"srv1:",
		"srv1:not-a-port",
		"srv1:0",
		"srv1:65536",
		"[::1",
		"[]",
		"[localhost]",
		"[::1]:not-a-port",
		"2001:db8::1:2222:invalid",
	}
	for _, address := range tests {
		t.Run(address, func(t *testing.T) {
			conn, err := NewServerConnection(
				address,
				"user",
				nil,
				testHostKeyCallback{},
				&mockHandler{},
				nil,
				sessionspec.Spec{},
				false,
				"",
				false,
				nil,
				logging.NopLogger{},
			)
			if conn != nil {
				t.Fatalf("connection = %#v, want nil", conn)
			}
			if err == nil || !strings.Contains(err.Error(), address) {
				t.Fatalf("NewServerConnection(%q) error = %v, want address parse error", address, err)
			}
		})
	}
}

func TestNewServerConnectionParsesAddressForms(t *testing.T) {

	tests := []struct {
		address      string
		wantHost     string
		wantPort     int
		wantDialAddr string
	}{
		{address: "server.example", wantHost: "server.example", wantPort: 3022, wantDialAddr: "server.example:3022"},
		{address: "server.example:2022", wantHost: "server.example", wantPort: 2022, wantDialAddr: "server.example:2022"},
		{address: "127.0.0.1", wantHost: "127.0.0.1", wantPort: 3022, wantDialAddr: "127.0.0.1:3022"},
		{address: "[::1]:2222", wantHost: "::1", wantPort: 2222, wantDialAddr: "[::1]:2222"},
		{address: "::1", wantHost: "::1", wantPort: 3022, wantDialAddr: "[::1]:3022"},
		{address: "[::1]", wantHost: "::1", wantPort: 3022, wantDialAddr: "[::1]:3022"},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			conn, err := NewServerConnection(
				tt.address,
				"user",
				nil,
				testHostKeyCallback{},
				&mockHandler{},
				nil,
				sessionspec.Spec{},
				false,
				"",
				false,
				testSSHSettings{port: 3022},
				logging.NopLogger{},
			)
			if err != nil {
				t.Fatalf("NewServerConnection(%q): %v", tt.address, err)
			}
			if conn.hostname != tt.wantHost || conn.port != tt.wantPort {
				t.Fatalf("parsed address = %q:%d, want %q:%d", conn.hostname, conn.port, tt.wantHost, tt.wantPort)
			}
			if got := net.JoinHostPort(conn.hostname, strconv.Itoa(conn.port)); got != tt.wantDialAddr {
				t.Fatalf("dial address = %q, want %q", got, tt.wantDialAddr)
			}
		})
	}
}

func TestNewServerConnectionRejectsInvalidConfiguredDefaultPort(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			conn, err := NewServerConnection(
				"server.example",
				"user",
				nil,
				testHostKeyCallback{},
				&mockHandler{},
				nil,
				sessionspec.Spec{},
				false,
				"",
				false,
				testSSHSettings{port: port},
				logging.NopLogger{},
			)
			if conn != nil {
				t.Fatalf("connection = %#v, want nil", conn)
			}
			if err == nil || !strings.Contains(err.Error(), "default port must be between 1 and 65535") {
				t.Fatalf("NewServerConnection default port %d error = %v, want port range error", port, err)
			}
		})
	}
}

func TestServerConnectionSupportsQueryUpdates(t *testing.T) {

	conn := &ServerConnection{
		handler: &mockHandler{
			waitForCapabilities: true,
			capabilities: map[string]bool{
				protocol.CapabilityQueryUpdateV1: true,
			},
		},
	}

	if !conn.SupportsQueryUpdates(10 * time.Millisecond) {
		t.Fatalf("expected query-update capability to be detected")
	}
}

func TestServerConnectionSupportsQueryUpdatesFallsBackForOlderServers(t *testing.T) {

	conn := &ServerConnection{
		handler: &mockHandler{},
	}

	if conn.SupportsQueryUpdates(5 * time.Millisecond) {
		t.Fatalf("expected old-server fallback when no capability is advertised")
	}
}

func TestServerConnectionSupportsQueryUpdatesRequiresCapabilityFlag(t *testing.T) {

	conn := &ServerConnection{
		handler: &mockHandler{
			waitForCapabilities: true,
		},
	}

	if conn.SupportsQueryUpdates(10 * time.Millisecond) {
		t.Fatalf("expected capability wait success alone to be insufficient")
	}
}

func TestServerConnectionApplySessionSpecStart(t *testing.T) {

	conn := &ServerConnection{
		server: "srv1",
		handler: &mockHandler{
			waitForCapabilities: true,
			capabilities: map[string]bool{
				protocol.CapabilityQueryUpdateV1: true,
			},
			sessionAcks: []handlers.SessionAck{{
				Action:     "start",
				Generation: 1,
			}},
		},
	}

	spec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	if err := conn.ApplySessionSpec(spec, 10*time.Millisecond); err != nil {
		t.Fatalf("ApplySessionSpec() error = %v", err)
	}

	mock := conn.handler.(*mockHandler)
	if len(mock.commands) != 1 {
		t.Fatalf("expected one session command, got %d", len(mock.commands))
	}
	if committedSpec, generation, ok := conn.CommittedSession(); !ok || generation != 1 || committedSpec.Regex != "ERROR" {
		t.Fatalf("unexpected committed session: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
}

func TestServerConnectionApplySessionSpecUpdateUsesNextGeneration(t *testing.T) {

	mock := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
		sessionAcks: []handlers.SessionAck{
			{Action: "start", Generation: 4},
			{Action: "update", Generation: 5},
		},
	}
	conn := &ServerConnection{
		server:  "srv1",
		handler: mock,
	}

	startSpec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	updateSpec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "WARN",
	}

	if err := conn.ApplySessionSpec(startSpec, 10*time.Millisecond); err != nil {
		t.Fatalf("start ApplySessionSpec() error = %v", err)
	}
	if err := conn.ApplySessionSpec(updateSpec, 10*time.Millisecond); err != nil {
		t.Fatalf("update ApplySessionSpec() error = %v", err)
	}
	if len(mock.commands) != 2 {
		t.Fatalf("expected two session commands, got %d", len(mock.commands))
	}
	if committedSpec, generation, ok := conn.CommittedSession(); !ok || generation != 5 || committedSpec.Regex != "WARN" {
		t.Fatalf("unexpected committed session after update: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
}

func TestServerConnectionApplySessionSpecReappliesPreviousSpecForRollback(t *testing.T) {

	mock := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
		sessionAcks: []handlers.SessionAck{
			{Action: "start", Generation: 4},
			{Action: "update", Generation: 5},
			{Action: "update", Generation: 6},
		},
	}
	conn := &ServerConnection{
		server:  "srv1",
		handler: mock,
	}

	startSpec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	updateSpec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "WARN",
	}

	if err := conn.ApplySessionSpec(startSpec, 10*time.Millisecond); err != nil {
		t.Fatalf("start ApplySessionSpec() error = %v", err)
	}
	if err := conn.ApplySessionSpec(updateSpec, 10*time.Millisecond); err != nil {
		t.Fatalf("update ApplySessionSpec() error = %v", err)
	}
	if err := conn.ApplySessionSpec(startSpec, 10*time.Millisecond); err != nil {
		t.Fatalf("rollback ApplySessionSpec() error = %v", err)
	}
	if len(mock.commands) != 3 {
		t.Fatalf("expected three session commands, got %d", len(mock.commands))
	}
	if committedSpec, generation, ok := conn.CommittedSession(); !ok || generation != 6 || committedSpec.Regex != "ERROR" {
		t.Fatalf("unexpected committed session after rollback: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
}

func TestServerConnectionApplySessionSpecFallsBackForUnsupportedServer(t *testing.T) {

	conn := &ServerConnection{
		handler: &mockHandler{},
	}

	err := conn.ApplySessionSpec(sessionspec.Spec{Mode: omode.TailClient, Regex: "ERROR"}, 5*time.Millisecond)
	if !errors.Is(err, ErrSessionUnsupported) {
		t.Fatalf("expected ErrSessionUnsupported, got %v", err)
	}
}

func TestRequireJournalCapability(t *testing.T) {
	tests := []struct {
		name                string
		spec                sessionspec.Spec
		waitForCapabilities bool
		capabilities        map[string]bool
		wantErr             error
		wantServerError     bool
	}{
		{
			name: "journal file with journal capability",
			spec: sessionspec.Spec{
				Mode:  omode.CatClient,
				Files: []string{"journal:ssh.service"},
			},
			waitForCapabilities: true,
			capabilities: map[string]bool{
				protocol.CapabilityJournalV1: true,
			},
		},
		{
			name: "journal file without journal capability",
			spec: sessionspec.Spec{
				Mode:  omode.CatClient,
				Files: []string{"journal:ssh.service"},
			},
			waitForCapabilities: true,
			capabilities: map[string]bool{
				protocol.CapabilityQueryUpdateV1: true,
			},
			wantErr:         ErrJournalUnsupported,
			wantServerError: true,
		},
		{
			name: "journal file without capabilities advertisement",
			spec: sessionspec.Spec{
				Mode:  omode.CatClient,
				Files: []string{"journal:ssh.service"},
			},
			wantErr:         ErrJournalUnsupported,
			wantServerError: true,
		},
		{
			name: "regular file without journal capability",
			spec: sessionspec.Spec{
				Mode:  omode.CatClient,
				Files: []string{"/var/log/app.log"},
			},
			waitForCapabilities: true,
			capabilities: map[string]bool{
				protocol.CapabilityQueryUpdateV1: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := &mockHandler{
				waitForCapabilities: tc.waitForCapabilities,
				capabilities:        tc.capabilities,
			}

			err := requireJournalCapability("srv1", handler, tc.spec, 10*time.Millisecond)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("requireJournalCapability() error = %v, want %v", err, tc.wantErr)
			}
			if got := handler.serverError != ""; got != tc.wantServerError {
				t.Fatalf("server error recorded = %v, want %v", got, tc.wantServerError)
			}
			if tc.wantServerError && !strings.Contains(handler.serverError, protocol.CapabilityJournalV1) {
				t.Fatalf("server error %q does not mention %s", handler.serverError, protocol.CapabilityJournalV1)
			}
		})
	}
}

func TestDispatchInitialCommandsRejectsJournalWithoutCapability(t *testing.T) {

	handler := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
	}
	spec := sessionspec.Spec{
		Mode:  omode.CatClient,
		Files: []string{"journal:ssh.service"},
	}

	err := dispatchInitialCommands("srv1", handler, []string{"cat: journal:ssh.service ."}, false,
		spec, &committedSessionState{}, logging.NopLogger{})
	if !errors.Is(err, ErrJournalUnsupported) {
		t.Fatalf("expected ErrJournalUnsupported, got %v", err)
	}
	if len(handler.commands) != 0 {
		t.Fatalf("expected no commands to be sent, got %#v", handler.commands)
	}
	if handler.Status() != 1 {
		t.Fatalf("handler status = %d, want 1", handler.Status())
	}
}

func TestDispatchInitialCommandsRejectsInteractiveJournalWithoutCapability(t *testing.T) {

	handler := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
	}
	spec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"journal:ssh.service"},
	}

	err := dispatchInitialCommands("srv1", handler, []string{"tail: journal:ssh.service ."}, true,
		spec, &committedSessionState{}, logging.NopLogger{})
	if !errors.Is(err, ErrJournalUnsupported) {
		t.Fatalf("expected ErrJournalUnsupported, got %v", err)
	}
	if len(handler.commands) != 0 {
		t.Fatalf("expected no commands to be sent, got %#v", handler.commands)
	}
	if handler.Status() != 1 {
		t.Fatalf("handler status = %d, want 1", handler.Status())
	}
}

func TestServerConnectionApplySessionSpecPreservesCommittedStateOnRejectedUpdate(t *testing.T) {

	mock := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
		sessionAcks: []handlers.SessionAck{
			{Action: "start", Generation: 2},
			{Action: "error", Error: "bad reload"},
		},
	}
	conn := &ServerConnection{
		server:  "srv1",
		handler: mock,
	}

	startSpec := sessionspec.Spec{Mode: omode.TailClient, Regex: "ERROR"}
	if err := conn.ApplySessionSpec(startSpec, 10*time.Millisecond); err != nil {
		t.Fatalf("start ApplySessionSpec() error = %v", err)
	}

	err := conn.ApplySessionSpec(sessionspec.Spec{Mode: omode.TailClient, Regex: "WARN"}, 10*time.Millisecond)
	if !errors.Is(err, ErrSessionRejected) {
		t.Fatalf("expected ErrSessionRejected, got %v", err)
	}
	if committedSpec, generation, ok := conn.CommittedSession(); !ok || generation != 2 || committedSpec.Regex != "ERROR" {
		t.Fatalf("unexpected committed session after rejected update: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
}

func TestServerConnectionApplySessionSpecRejectsUnexpectedAck(t *testing.T) {

	mock := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
		sessionAcks: []handlers.SessionAck{
			{Action: "update", Generation: 1},
		},
	}
	conn := &ServerConnection{
		server:  "srv1",
		handler: mock,
	}

	err := conn.ApplySessionSpec(sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}, 10*time.Millisecond)
	if !errors.Is(err, ErrUnexpectedSessionAck) {
		t.Fatalf("expected ErrUnexpectedSessionAck, got %v", err)
	}
	if _, _, ok := conn.CommittedSession(); ok {
		t.Fatalf("unexpected committed session after mismatched ack")
	}
}

func TestServerConnectionApplySessionSpecTimesOutWaitingForAck(t *testing.T) {

	mock := &mockHandler{
		waitForCapabilities: true,
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
	}
	conn := &ServerConnection{
		server:  "srv1",
		handler: mock,
	}

	err := conn.ApplySessionSpec(sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}, 10*time.Millisecond)
	if !errors.Is(err, ErrSessionAckTimeout) {
		t.Fatalf("expected ErrSessionAckTimeout, got %v", err)
	}
	if len(mock.commands) != 1 {
		t.Fatalf("expected session command to be sent before timeout, got %d", len(mock.commands))
	}
	if _, _, ok := conn.CommittedSession(); ok {
		t.Fatalf("unexpected committed session after missing ack")
	}
}

func TestApplySessionSpecSerializesConcurrentBootstrapAndReload(t *testing.T) {

	handler := newBlockingSessionHandler()
	state := &committedSessionState{}

	initialSpec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	reloadSpec := sessionspec.Spec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "WARN",
	}

	initialErrCh := make(chan error, 1)
	go func() {
		initialErrCh <- dispatchInitialCommands("srv1", handler, nil, true, initialSpec, state, logging.NopLogger{})
	}()

	firstCommand := <-handler.commandsCh
	if !strings.HasPrefix(firstCommand, "SESSION START ") {
		t.Fatalf("expected initial SESSION START command, got %q", firstCommand)
	}

	reloadErrCh := make(chan error, 1)
	go func() {
		reloadErrCh <- applySessionSpec("srv1", handler, state, reloadSpec, 50*time.Millisecond, logging.NopLogger{})
	}()

	select {
	case command := <-handler.commandsCh:
		t.Fatalf("unexpected concurrent session command before bootstrap ack: %q", command)
	case <-time.After(10 * time.Millisecond):
	}

	handler.ackCh <- handlers.SessionAck{Action: "start", Generation: 1}
	if err := <-initialErrCh; err != nil {
		t.Fatalf("dispatchInitialCommands() error = %v", err)
	}

	secondCommand := <-handler.commandsCh
	if !strings.HasPrefix(secondCommand, "SESSION UPDATE 2 ") {
		t.Fatalf("expected reload to send SESSION UPDATE after bootstrap, got %q", secondCommand)
	}

	handler.ackCh <- handlers.SessionAck{Action: "update", Generation: 2}
	if err := <-reloadErrCh; err != nil {
		t.Fatalf("applySessionSpec() error = %v", err)
	}

	committedSpec, generation, ok := state.snapshot()
	if !ok || generation != 2 || committedSpec.Regex != "WARN" {
		t.Fatalf("unexpected committed session after reload: spec=%#v generation=%d ok=%v", committedSpec, generation, ok)
	}
}

// TestThrottleReleasedIsIdempotent verifies that calling the throttle-release
// logic from two concurrent goroutines drains throttleCh exactly once and does
// not panic or block.  This is a regression test for the data race that existed
// when the old bool guard (throttlingDone) was read and written without
// synchronization: under -race two goroutines could both observe the bool as
// false and both attempt to drain the channel, stealing an extra slot.
func TestThrottleReleasedIsIdempotent(t *testing.T) {
	t.Parallel()

	// throttleCh is buffered with 1 slot, as in the real Start() path.
	throttleCh := make(chan struct{}, 1)
	throttleCh <- struct{}{} // occupy the one slot

	conn := &ServerConnection{}

	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)

	// Simulate workers racing to release the throttle slot (e.g. handle()
	// early-release and the defer cleanup in Start() firing around the same
	// time).  Only one drain must succeed; the rest must be no-ops.
	for range workers {
		go func() {
			defer wg.Done()
			conn.throttleReleased.Do(func() {
				<-throttleCh
			})
		}()
	}

	wg.Wait()

	// throttleCh must be empty: exactly one goroutine drained it.
	if len(throttleCh) != 0 {
		t.Fatalf("throttleCh length = %d, want 0 (slot was not released)", len(throttleCh))
	}

	// Confirm a second occupant can now be added, proving the slot is free.
	select {
	case throttleCh <- struct{}{}:
		// expected: slot was freed exactly once
	default:
		t.Fatal("throttleCh full after release, expected one free slot")
	}
}

func TestServerConnectionHandleFlushesAfterStdoutCopyAndWaitsForSession(t *testing.T) {

	allowWrite := make(chan struct{})
	handler := newLifecycleHandler(allowWrite)
	session := newLifecycleSession([]byte("aggregate payload"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	throttleCh := make(chan struct{}, 1)
	throttleCh <- struct{}{}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		authKeyDisabled: true,
	}
	handleDone := make(chan error, 1)
	go func() {
		handleDone <- conn.handle(ctx, cancel, session, throttleCh)
	}()

	waitForSignal(t, handler.writeStarted, "stdout handler Write to start")
	cancel()
	waitForSignal(t, session.closed, "session to close after cancellation")

	select {
	case <-handler.shutdown:
		t.Fatal("handler shut down while its stdout Write was still in flight")
	default:
	}

	close(allowWrite)
	waitForSignal(t, handler.shutdown, "handler shutdown after stdout copy")

	select {
	case err := <-handleDone:
		t.Fatalf("handle returned before Session.Wait completed: %v", err)
	default:
	}

	close(session.allowWait)
	select {
	case err := <-handleDone:
		if err != nil {
			t.Fatalf("handle() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handle did not return after stdout copy and Session.Wait completed")
	}

	if got := handler.shutdownCalls(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", got)
	}
	if handler.shutdownDuringWrite() {
		t.Fatal("Shutdown ran before the final stdout Write completed")
	}
}

func TestServerConnectionHandleCompletesForNormalEOFAndHandlerDone(t *testing.T) {
	tests := []struct {
		name    string
		trigger func(*lifecycleHandler, *lifecycleSession)
	}{
		{
			name: "normal stdout EOF",
			trigger: func(_ *lifecycleHandler, session *lifecycleSession) {
				session.finish()
			},
		},
		{
			name: "handler done",
			trigger: func(handler *lifecycleHandler, _ *lifecycleSession) {
				handler.signalDone()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			allowWrite := make(chan struct{})
			close(allowWrite)
			handler := newLifecycleHandler(allowWrite)
			session := newLifecycleSession([]byte("payload"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			throttleCh := make(chan struct{}, 1)
			throttleCh <- struct{}{}
			conn := &ServerConnection{
				server:          "srv1",
				handler:         handler,
				authKeyDisabled: true,
			}

			handleDone := make(chan error, 1)
			go func() {
				handleDone <- conn.handle(ctx, cancel, session, throttleCh)
			}()
			waitForSignal(t, handler.writeFinished, "stdout handler Write to finish")
			tt.trigger(handler, session)
			close(session.allowWait)

			select {
			case err := <-handleDone:
				if err != nil {
					t.Fatalf("handle() error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("handle did not complete")
			}
			if got := handler.shutdownCalls(); got != 1 {
				t.Fatalf("Shutdown calls = %d, want 1", got)
			}
		})
	}
}

func TestServerConnectionHandleCancelsTransportAfterIndependentStdoutEOF(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allowWrite := make(chan struct{})
	close(allowWrite)
	handler := newLifecycleHandler(allowWrite)
	session := newIndependentEOFSession(ctx.Done())
	throttleCh := make(chan struct{}, 1)
	throttleCh <- struct{}{}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		authKeyDisabled: true,
	}

	handleDone := make(chan error, 1)
	go func() {
		handleDone <- conn.handle(ctx, cancel, session, throttleCh)
	}()

	waitForSignal(t, session.stdout.readStarted, "stdout copy to start")
	close(session.stdout.eof)

	select {
	case err := <-handleDone:
		if err != nil {
			t.Fatalf("handle() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handle did not cancel the transport after stdout EOF")
	}
	waitForSignal(t, ctx.Done(), "connection context cancellation")
	waitForSignal(t, session.closeCalled, "SSH session close")

	if got := handler.shutdownCalls(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", got)
	}
}

func TestServerConnectionStartWaitsForDialCleanup(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	conn := &ServerConnection{
		server:          "srv1",
		handler:         &mockHandler{},
		hostKeyCallback: testHostKeyCallback{},
		dialFn: func(context.Context, context.CancelFunc, chan struct{}, chan struct{}) error {
			close(dialStarted)
			<-releaseDial
			return nil
		},
	}
	throttleCh := make(chan struct{}, 1)
	statsCh := make(chan struct{}, 1)
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		conn.Start(ctx, cancel, throttleCh, statsCh)
	}()

	waitForSignal(t, dialStarted, "dial worker to start")
	cancel()
	select {
	case <-startDone:
		t.Fatal("Start returned before the dial worker cleaned up")
	default:
	}

	close(releaseDial)
	waitForSignal(t, startDone, "Start to return after dial cleanup")
	if got := len(throttleCh); got != 0 {
		t.Fatalf("throttle channel length = %d, want 0 after cleanup", got)
	}
}

func TestServerConnectionStartReportsDialFailure(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &mockHandler{}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		hostKeyCallback: testHostKeyCallback{},
		dialFn: func(context.Context, context.CancelFunc, chan struct{}, chan struct{}) error {
			return errors.New("dial failed")
		},
	}

	conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))
	if handler.status != 1 {
		t.Fatalf("handler status = %d, want 1", handler.status)
	}
	if !strings.Contains(handler.serverError, "dial failed") {
		t.Fatalf("server error = %q, want dial failure", handler.serverError)
	}
}

func TestServerConnectionStartReportsDialTimeoutWhileContextActive(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &mockHandler{}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		hostKeyCallback: testHostKeyCallback{},
		dialFn: func(context.Context, context.CancelFunc, chan struct{}, chan struct{}) error {
			return context.DeadlineExceeded
		},
	}

	conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))
	if handler.status != 1 {
		t.Fatalf("handler status = %d, want 1", handler.status)
	}
	if handler.serverErrorCalls != 1 {
		t.Fatalf("server error reports = %d, want 1", handler.serverErrorCalls)
	}
}

func TestServerConnectionStartDoesNotReportContextCancellation(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	dialStarted := make(chan struct{})
	handler := &mockHandler{}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		hostKeyCallback: testHostKeyCallback{},
		dialFn: func(ctx context.Context, _ context.CancelFunc, _ chan struct{}, _ chan struct{}) error {
			close(dialStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))
	}()
	waitForSignal(t, dialStarted, "dial worker to start")
	cancel()
	waitForSignal(t, startDone, "Start to return after cancellation")

	if handler.status != 0 {
		t.Fatalf("handler status = %d, want 0 after context cancellation", handler.status)
	}
	if handler.serverErrorCalls != 0 {
		t.Fatalf("server error reports = %d, want 0 after context cancellation", handler.serverErrorCalls)
	}
}

func TestServerConnectionStartDoesNotDuplicateJournalCapabilityError(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &mockHandler{waitForCapabilities: true}
	spec := sessionspec.Spec{Mode: omode.CatClient, Files: []string{"journal:ssh.service"}}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		hostKeyCallback: testHostKeyCallback{},
	}
	conn.dialFn = func(context.Context, context.CancelFunc, chan struct{}, chan struct{}) error {
		return dispatchInitialCommands(conn.server, handler, nil, false, spec,
			&conn.sessionState, logging.NopLogger{})
	}

	conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))
	if handler.status != 1 {
		t.Fatalf("handler status = %d, want 1", handler.status)
	}
	if handler.serverErrorCalls != 1 {
		t.Fatalf("server error reports = %d, want exactly 1", handler.serverErrorCalls)
	}
	if !strings.Contains(handler.serverError, protocol.CapabilityJournalV1) {
		t.Fatalf("server error = %q, want journal capability error", handler.serverError)
	}
}

func TestServerConnectionStartReportsShellRejection(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &mockHandler{}
	session := &shellRejectSession{}
	conn := &ServerConnection{
		server:          "srv1",
		handler:         handler,
		hostKeyCallback: testHostKeyCallback{},
		authKeyDisabled: true,
	}
	conn.dialFn = func(ctx context.Context, cancel context.CancelFunc,
		throttleCh, _ chan struct{}) error {
		return conn.handle(ctx, cancel, session, throttleCh)
	}

	conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))
	if handler.status != 1 {
		t.Fatalf("handler status = %d, want 1", handler.status)
	}
	if !strings.Contains(handler.serverError, "failed to start SSH shell") ||
		!strings.Contains(handler.serverError, "shell rejected") {
		t.Fatalf("server error = %q, want wrapped shell rejection", handler.serverError)
	}
	if session.closeCalls != 1 {
		t.Fatalf("session Close calls = %d, want 1", session.closeCalls)
	}
}

type testSSHSettings struct {
	port    int
	timeout time.Duration
}

type lifecycleHandler struct {
	done           chan struct{}
	doneOnce       sync.Once
	writeStarted   chan struct{}
	writeStartOne  sync.Once
	writeFinished  chan struct{}
	writeFinishOne sync.Once
	allowWrite     <-chan struct{}
	shutdown       chan struct{}
	shutdownOnce   sync.Once
	mu             sync.Mutex
	writeActive    bool
	shutdownCount  int
	badShutdown    bool
}

func newLifecycleHandler(allowWrite <-chan struct{}) *lifecycleHandler {
	return &lifecycleHandler{
		done:          make(chan struct{}),
		writeStarted:  make(chan struct{}),
		writeFinished: make(chan struct{}),
		allowWrite:    allowWrite,
		shutdown:      make(chan struct{}),
	}
}

var _ handlers.Handler = (*lifecycleHandler)(nil)

func (*lifecycleHandler) Capabilities() []string                 { return nil }
func (*lifecycleHandler) HasCapability(string) bool              { return false }
func (*lifecycleHandler) ReportServerError(string)               {}
func (*lifecycleHandler) SendMessage(string) error               { return nil }
func (*lifecycleHandler) Server() string                         { return "lifecycle" }
func (*lifecycleHandler) Status() int                            { return 0 }
func (h *lifecycleHandler) Done() <-chan struct{}                { return h.done }
func (*lifecycleHandler) WaitForCapabilities(time.Duration) bool { return false }
func (*lifecycleHandler) WaitForSessionAck(time.Duration) (handlers.SessionAck, bool) {
	return handlers.SessionAck{}, false
}

func (h *lifecycleHandler) Read([]byte) (int, error) {
	<-h.done
	return 0, io.EOF
}

func (h *lifecycleHandler) Write(p []byte) (int, error) {
	h.mu.Lock()
	h.writeActive = true
	h.mu.Unlock()
	h.writeStartOne.Do(func() { close(h.writeStarted) })
	<-h.allowWrite
	h.mu.Lock()
	h.writeActive = false
	h.mu.Unlock()
	h.writeFinishOne.Do(func() { close(h.writeFinished) })
	return len(p), nil
}

func (h *lifecycleHandler) Shutdown() {
	h.mu.Lock()
	h.shutdownCount++
	if h.writeActive {
		h.badShutdown = true
	}
	h.mu.Unlock()
	h.signalDone()
	h.shutdownOnce.Do(func() { close(h.shutdown) })
}

func (h *lifecycleHandler) signalDone() {
	h.doneOnce.Do(func() { close(h.done) })
}

func (h *lifecycleHandler) shutdownCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shutdownCount
}

func (h *lifecycleHandler) shutdownDuringWrite() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.badShutdown
}

type lifecycleSession struct {
	stdout    *lifecycleReader
	closed    chan struct{}
	closeOnce sync.Once
	allowWait chan struct{}
}

type shellRejectSession struct{ closeCalls int }

var _ sshSession = (*shellRejectSession)(nil)

func (*shellRejectSession) StdinPipe() (io.WriteCloser, error) {
	return lifecycleWriteCloser{Writer: io.Discard}, nil
}

func (*shellRejectSession) StdoutPipe() (io.Reader, error) { return strings.NewReader(""), nil }
func (*shellRejectSession) Shell() error                   { return errors.New("shell rejected") }
func (*shellRejectSession) Wait() error                    { return nil }
func (s *shellRejectSession) Close() error {
	s.closeCalls++
	return nil
}

func newLifecycleSession(payload []byte) *lifecycleSession {
	closed := make(chan struct{})
	return &lifecycleSession{
		stdout:    &lifecycleReader{payload: payload, closed: closed},
		closed:    closed,
		allowWait: make(chan struct{}),
	}
}

var _ sshSession = (*lifecycleSession)(nil)

func (*lifecycleSession) StdinPipe() (io.WriteCloser, error) {
	return lifecycleWriteCloser{Writer: io.Discard}, nil
}

func (s *lifecycleSession) StdoutPipe() (io.Reader, error) { return s.stdout, nil }
func (*lifecycleSession) Shell() error                     { return nil }

func (s *lifecycleSession) Wait() error {
	<-s.closed
	<-s.allowWait
	return nil
}

func (s *lifecycleSession) Close() error {
	s.finish()
	return nil
}

func (s *lifecycleSession) finish() {
	s.closeOnce.Do(func() { close(s.closed) })
}

type lifecycleReader struct {
	payload []byte
	closed  <-chan struct{}
}

func (r *lifecycleReader) Read(p []byte) (int, error) {
	if len(r.payload) > 0 {
		n := copy(p, r.payload)
		r.payload = r.payload[n:]
		return n, nil
	}
	<-r.closed
	return 0, io.EOF
}

type lifecycleWriteCloser struct{ io.Writer }

func (lifecycleWriteCloser) Close() error { return nil }

type independentEOFSession struct {
	stdout      *independentEOFReader
	transport   <-chan struct{}
	closeCalled chan struct{}
	closeOnce   sync.Once
}

func newIndependentEOFSession(transport <-chan struct{}) *independentEOFSession {
	return &independentEOFSession{
		stdout: &independentEOFReader{
			readStarted: make(chan struct{}),
			eof:         make(chan struct{}),
		},
		transport:   transport,
		closeCalled: make(chan struct{}),
	}
}

var _ sshSession = (*independentEOFSession)(nil)

func (*independentEOFSession) StdinPipe() (io.WriteCloser, error) {
	return lifecycleWriteCloser{Writer: io.Discard}, nil
}

func (s *independentEOFSession) StdoutPipe() (io.Reader, error) { return s.stdout, nil }
func (*independentEOFSession) Shell() error                     { return nil }

func (s *independentEOFSession) Wait() error {
	<-s.transport
	return nil
}

func (s *independentEOFSession) Close() error {
	s.closeOnce.Do(func() { close(s.closeCalled) })
	return nil
}

type independentEOFReader struct {
	readStarted chan struct{}
	readOnce    sync.Once
	eof         chan struct{}
}

func (r *independentEOFReader) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.readStarted) })
	<-r.eof
	return 0, io.EOF
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func (s testSSHSettings) SSHPort() int {
	return s.port
}

func (s testSSHSettings) SSHConnectTimeout() time.Duration {
	return s.timeout
}

type testHostKeyCallback struct{}

func (testHostKeyCallback) Wrap(context.Context) ssh.HostKeyCallback {
	return ssh.InsecureIgnoreHostKey()
}

func (testHostKeyCallback) Untrusted(string) bool {
	return false
}

func (testHostKeyCallback) PromptAddHosts(context.Context) {}

type mockHandler struct {
	commands            []string
	capabilities        map[string]bool
	waitForCapabilities bool
	sessionAcks         []handlers.SessionAck
	serverError         string
	serverErrorCalls    int
	status              int
}

var _ handlers.Handler = (*mockHandler)(nil)

func (m *mockHandler) SendMessage(command string) error {
	m.commands = append(m.commands, command)
	return nil
}

func (m *mockHandler) Capabilities() []string {
	var capabilities []string
	for capability := range m.capabilities {
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func (m *mockHandler) HasCapability(name string) bool {
	return m.capabilities[name]
}

func (m *mockHandler) ReportServerError(message string) {
	m.serverErrorCalls++
	m.serverError = message
	m.status = 1
}

func (m *mockHandler) Server() string {
	return "mock"
}

func (m *mockHandler) Status() int {
	return m.status
}

func (m *mockHandler) Shutdown() {}

func (m *mockHandler) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (m *mockHandler) WaitForCapabilities(timeout time.Duration) bool {
	return m.waitForCapabilities
}

func (m *mockHandler) WaitForSessionAck(timeout time.Duration) (handlers.SessionAck, bool) {
	if timeout <= 0 {
		return handlers.SessionAck{}, false
	}
	if len(m.sessionAcks) == 0 {
		return handlers.SessionAck{}, false
	}

	ack := m.sessionAcks[0]
	m.sessionAcks = m.sessionAcks[1:]
	return ack, true
}

func (m *mockHandler) Read(_ []byte) (int, error) {
	return 0, nil
}

func (m *mockHandler) Write(p []byte) (int, error) {
	return len(p), nil
}

type blockingSessionHandler struct {
	mu           sync.Mutex
	commands     []string
	commandsCh   chan string
	ackCh        chan handlers.SessionAck
	capabilities map[string]bool
}

func newBlockingSessionHandler() *blockingSessionHandler {
	return &blockingSessionHandler{
		commandsCh: make(chan string, 8),
		ackCh:      make(chan handlers.SessionAck, 8),
		capabilities: map[string]bool{
			protocol.CapabilityQueryUpdateV1: true,
		},
	}
}

var _ handlers.Handler = (*blockingSessionHandler)(nil)

func (h *blockingSessionHandler) SendMessage(command string) error {
	h.mu.Lock()
	h.commands = append(h.commands, command)
	h.mu.Unlock()
	h.commandsCh <- command
	return nil
}

func (h *blockingSessionHandler) Capabilities() []string {
	capabilities := make([]string, 0, len(h.capabilities))
	for capability := range h.capabilities {
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func (h *blockingSessionHandler) HasCapability(name string) bool {
	return h.capabilities[name]
}

func (*blockingSessionHandler) ReportServerError(string) {}

func (*blockingSessionHandler) Server() string {
	return "mock"
}

func (*blockingSessionHandler) Status() int {
	return 0
}

func (*blockingSessionHandler) Shutdown() {}

func (*blockingSessionHandler) Done() <-chan struct{} {
	return make(chan struct{})
}

func (*blockingSessionHandler) WaitForCapabilities(time.Duration) bool {
	return true
}

func (h *blockingSessionHandler) WaitForSessionAck(timeout time.Duration) (handlers.SessionAck, bool) {
	if timeout <= 0 {
		select {
		case ack := <-h.ackCh:
			return ack, true
		default:
			return handlers.SessionAck{}, false
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ack := <-h.ackCh:
		return ack, true
	case <-timer.C:
		return handlers.SessionAck{}, false
	}
}

func (*blockingSessionHandler) Read(_ []byte) (int, error) {
	return 0, nil
}

func (*blockingSessionHandler) Write(p []byte) (int, error) {
	return len(p), nil
}
