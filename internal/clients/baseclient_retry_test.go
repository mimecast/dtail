package clients

import (
	"context"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
	sshclient "github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

func TestNextRetryDelay(t *testing.T) {
	tests := []struct {
		name    string
		current time.Duration
		want    time.Duration
	}{
		{name: "zero uses initial", current: 0, want: initialRetryDelay},
		{name: "doubles normally", current: 4 * time.Second, want: 8 * time.Second},
		{name: "caps at max", current: 40 * time.Second, want: maxRetryDelay},
		{name: "stays max at max", current: maxRetryDelay, want: maxRetryDelay},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextRetryDelay(tt.current); got != tt.want {
				t.Fatalf("nextRetryDelay(%v) = %v, want %v", tt.current, got, tt.want)
			}
		})
	}
}

func TestJitterRetryDelayWithinBounds(t *testing.T) {
	base := 10 * time.Second
	random := rand.New(rand.NewSource(1))

	minDelay := 8 * time.Second
	maxDelay := 12 * time.Second

	for i := 0; i < 100; i++ {
		got := jitterRetryDelay(base, random)
		if got < minDelay || got > maxDelay {
			t.Fatalf("jitterRetryDelay() = %v, expected between %v and %v", got, minDelay, maxDelay)
		}
	}
}

func TestSleepWithContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if sleepWithContext(ctx, time.Second) {
		t.Fatalf("sleepWithContext should stop when context is canceled")
	}

	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("sleepWithContext took too long to exit on canceled context")
	}
}

func TestStartConnectionReconnectsWithLatestSessionSpec(t *testing.T) {
	first := &retryTestConnector{
		server:  "srv1",
		handler: &retryTestHandler{},
	}
	second := &retryTestConnector{
		server:  "srv1",
		handler: &retryTestHandler{},
	}

	originalSpec := SessionSpec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	updatedSpec := SessionSpec{
		Mode:  omode.TailClient,
		Files: []string{"/var/log/next.log"},
		Regex: "WARN",
	}

	sleepCalls := 0
	var capturedSpec SessionSpec
	client := &baseClient{
		mu:          newBaseClientMu(),
		profile:     clientProfile{retry: true},
		sessionSpec: originalSpec,
		stats: &stats{
			connectionsEstCh: make(chan struct{}, 1),
		},
		connections: []connectors.Connector{first},
		connectionFactory: func(server string, _ []gossh.AuthMethod,
			_ sshclient.HostKeyCallback, sessionSpec SessionSpec, _ bool) connectors.Connector {
			if server != "srv1" {
				t.Fatalf("unexpected reconnect server %q", server)
			}
			capturedSpec = sessionSpec
			return second
		},
	}
	client.sleepFn = func(context.Context, time.Duration) bool {
		if sleepCalls == 0 {
			sleepCalls++
			client.sessionSpec = updatedSpec
			return true
		}
		return false
	}

	status := client.startConnection(context.Background(), 0, first)
	if status != 0 {
		t.Fatalf("startConnection() status = %d, want 0", status)
	}
	if capturedSpec.Regex != updatedSpec.Regex || len(capturedSpec.Files) != 1 || capturedSpec.Files[0] != updatedSpec.Files[0] {
		t.Fatalf("reconnect used stale session spec: got %#v want %#v", capturedSpec, updatedSpec)
	}
	if client.connections[0] != second {
		t.Fatalf("expected retried connector to replace the original connection")
	}
}

func TestApplyInteractiveReloadConcurrentWithReconnect(t *testing.T) {
	originalSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/app.log"},
		Regex: "ERROR",
	}
	nextSpec := SessionSpec{
		Mode:  omode.GrepClient,
		Files: []string{"/var/log/next.log"},
		Regex: "WARN",
	}
	originalArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/app.log",
		RegexStr: "ERROR",
	}
	nextArgs := config.Args{
		Mode:     omode.GrepClient,
		What:     "/var/log/next.log",
		RegexStr: "WARN",
	}

	var reconnects atomic.Int32
	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        originalArgs,
		profile:     clientProfile{retry: true},
		sessionSpec: originalSpec,
		stats: &stats{
			connectionsEstCh: make(chan struct{}, 1),
		},
		connections: []connectors.Connector{
			newReloadRetryConnector("srv1", originalSpec),
		},
		connectionFactory: func(server string, _ []gossh.AuthMethod,
			_ sshclient.HostKeyCallback, sessionSpec SessionSpec, _ bool) connectors.Connector {
			reconnects.Add(1)
			return newReloadRetryConnector(server, sessionSpec)
		},
	}

	client.sleepFn = func(context.Context, time.Duration) bool {
		return reconnects.Load() < 200
	}

	done := make(chan int, 1)
	go func() {
		done <- client.startConnection(context.Background(), 0, client.connections[0])
	}()

	for i := 0; i < 200; i++ {
		if err := client.applyInteractiveReload(nextArgs, nextSpec); err != nil {
			t.Fatalf("applyInteractiveReload() error = %v", err)
		}
		if err := client.applyInteractiveReload(originalArgs, originalSpec); err != nil {
			t.Fatalf("applyInteractiveReload() error = %v", err)
		}
	}

	if status := <-done; status != 0 {
		t.Fatalf("startConnection() status = %d, want 0", status)
	}
}

func newReloadRetryConnector(server string, spec SessionSpec) *reloadRetryConnector {
	return &reloadRetryConnector{
		interactiveReloadConnector: interactiveReloadConnector{
			server:        server,
			supported:     true,
			committedSpec: spec,
			liveSpec:      spec,
			generation:    1,
		},
		handler: &retryTestHandler{},
	}
}

type reloadRetryConnector struct {
	interactiveReloadConnector
	handler handlers.Handler
}

func (c *reloadRetryConnector) Handler() handlers.Handler { return c.handler }

type retryTestConnector struct {
	handler handlers.Handler
	server  string
}

func (c *retryTestConnector) Start(context.Context, context.CancelFunc, chan struct{}, chan struct{}) {
}

func (c *retryTestConnector) Server() string { return c.server }

func (c *retryTestConnector) Handler() handlers.Handler { return c.handler }

func (*retryTestConnector) SupportsQueryUpdates(time.Duration) bool { return false }

func (*retryTestConnector) ApplySessionSpec(SessionSpec, time.Duration) error { return nil }

func (*retryTestConnector) ApplySessionSpecWithGeneration(SessionSpec, uint64, time.Duration) error {
	return nil
}

func (*retryTestConnector) CommittedSession() (SessionSpec, uint64, bool) {
	return SessionSpec{}, 0, false
}

func (*retryTestConnector) RestoreCommittedSession(SessionSpec, uint64, bool) {}

type retryTestHandler struct{}

func (*retryTestHandler) Read([]byte) (int, error) { return 0, nil }

func (*retryTestHandler) Write(p []byte) (int, error) { return len(p), nil }

func (*retryTestHandler) Capabilities() []string { return nil }

func (*retryTestHandler) HasCapability(string) bool { return false }

func (*retryTestHandler) ReportServerError(string) {}

func (*retryTestHandler) SendMessage(string) error { return nil }

func (*retryTestHandler) Server() string { return "srv1" }

func (*retryTestHandler) Status() int { return 0 }

func (*retryTestHandler) Shutdown() {}

func (*retryTestHandler) Done() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (*retryTestHandler) WaitForCapabilities(time.Duration) bool { return false }

func (*retryTestHandler) WaitForSessionAck(time.Duration) (handlers.SessionAck, bool) {
	return handlers.SessionAck{}, false
}
