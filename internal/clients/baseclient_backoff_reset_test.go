package clients

import (
	"context"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	sshclient "github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

// reconnectDelays runs startConnection through sessions that each last
// sessionDuration and returns the backoff delays before the reconnects.
func reconnectDelays(t *testing.T, sessionDuration time.Duration, sessions int) []time.Duration {
	t.Helper()
	spec := SessionSpec{}
	client := &baseClient{
		mu:          newBaseClientMu(),
		profile:     clientProfile{retry: true},
		sessionSpec: spec,
		stats:       &stats{connectionsEstCh: make(chan struct{}, 1)},
		connections: []connectors.Connector{newReloadRetryConnector("srv1", spec)},
		connectionFactory: func(server string, _ []gossh.AuthMethod,
			_ sshclient.HostKeyCallback, sessionSpec SessionSpec, _ bool) connectors.Connector {
			return newReloadRetryConnector(server, sessionSpec)
		},
	}
	// The clock is read once before and once after every session.
	var clock time.Time
	client.nowFn = func() time.Time {
		clock = clock.Add(sessionDuration)
		return clock
	}
	var delays []time.Duration
	client.sleepFn = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		return len(delays) < sessions
	}
	client.startConnection(context.Background(), 0, client.connections[0])
	return delays
}

// The backoff grows while sessions end at once, and starts over after a
// session that ran for a while: a long-running dtail must not end up waiting
// a minute for every reconnect after a handful of dserver restarts.
func TestReconnectBackoffStartsOverAfterAHealthySession(t *testing.T) {
	short := reconnectDelays(t, time.Second, 8)
	if last := short[len(short)-1]; last < 4*initialRetryDelay {
		t.Fatalf("backoff did not grow for sessions ending at once: %v", short)
	}

	healthy := reconnectDelays(t, 2*maxRetryDelay, 8)
	for _, delay := range healthy {
		if delay > 2*initialRetryDelay {
			t.Fatalf("backoff grew although every session ran for a while: %v", healthy)
		}
	}
}
