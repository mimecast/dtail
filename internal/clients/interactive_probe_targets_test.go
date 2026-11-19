package clients

import (
	"context"
	"net"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

// probeTargetsSSHSettings are the SSH settings of the unreachable test
// connection.
type probeTargetsSSHSettings struct{}

func (probeTargetsSSHSettings) SSHPort() int                       { return 2222 }
func (probeTargetsSSHSettings) SSHHandshakeTimeout() time.Duration { return 30 * time.Second }
func (probeTargetsSSHSettings) SSHConnectTimeout() time.Duration   { return 2 * time.Second }

// unreachableServerConnection returns a real ServerConnection with a real
// ClientHandler to a local TCP port nothing listens on, and starts it: its
// dial is refused, so Start ends without a session, as for a server down
// in --servers.
func unreachableServerConnection(t *testing.T) (*connectors.ServerConnection, handlers.Handler) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatalf("close listener: %v", closeErr)
	}
	handler := handlers.NewClientHandler(address, nil)
	conn, err := connectors.NewServerConnection(address, "user", nil,
		interactiveReloadHostKeyCallback{}, handler, nil, SessionSpec{}, true, "", true,
		probeTargetsSSHSettings{}, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewServerConnection: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))
	return conn, handler
}

// TestProbeUnreachableFirstServer: a server that could not be connected to,
// first in --servers, neither holds up a probe round (and so :probe) nor
// takes the path listings: the round ends as soon as the healthy server
// answered, and the listing goes to the healthy server. Before, the dead
// connection's handler was never shut down, its probe waited the whole
// round timeout for a session that never came, and every listing went to
// it.
func TestProbeUnreachableFirstServer(t *testing.T) {
	down, _ := unreachableServerConnection(t)
	up, upHandler := probeTestConnection("up")
	upHandler.listings = map[string][]protocol.ProbeEntry{"/var/log/": {{Name: "app.log"}}}
	c, logger := newProbeTestClient([]string{"/var/log/app.log"}, down, up)
	c.prober.roundTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.prober.startListings(ctx)
	started := time.Now()
	result := c.prober.round(ctx)
	if elapsed := time.Since(started); elapsed > probeNowWait {
		t.Fatalf("round took %v, want less than :probe's wait of %v", elapsed, probeNowWait)
	}
	if !reflect.DeepEqual(result.answered, []string{"up"}) {
		t.Fatalf("round = %+v, want only up answered", result)
	}

	select {
	case <-c.prober.listPaths("/var/log/"):
	case <-time.After(pathListingWait):
		t.Fatalf("listing not done within %v", pathListingWait)
	}
	if listed := upHandler.listed(); !slices.Equal(listed, []string{"/var/log/"}) {
		t.Fatalf("up listed %q, want the listing", listed)
	}
	if match := c.prober.catalog.findListing("/var/log/", "", time.Now()); !match.complete {
		t.Fatalf("listing not cached: %+v", match)
	}
	cancel()
	c.prober.waitListings()
	logger.requireSilent(t)
}

// TestProbeTargetsPreferSessionsUp: connections whose session is up (the
// capabilities frame arrived) come first, in the client's order; ones still
// connecting only fill the slots left, and connections that ended or
// cannot probe are never asked. So a listing (the first target) goes to the
// first connection that is up, and to one still connecting only when none
// is up (the round at the session's start).
func TestProbeTargetsPreferSessionsUp(t *testing.T) {
	connecting := func(server string) *probeTestConnector {
		conn, handler := probeTestConnection(server)
		handler.connecting = true
		return conn
	}
	ended, endedHandler := probeTestConnection("ended")
	close(endedHandler.done)
	noProber := &probeTestConnector{server: "noprobe", handler: doneOnlyHandler{done: make(chan struct{})}}
	up1, _ := probeTestConnection("up1")
	up2, _ := probeTestConnection("up2")
	up3, _ := probeTestConnection("up3")

	tests := []struct {
		name  string
		conns []connectors.Connector
		want  []string
	}{
		{"up first", []connectors.Connector{connecting("c1"), ended, noProber, up1, up2},
			[]string{"up1", "up2", "c1"}},
		{"enough up", []connectors.Connector{connecting("c1"), up1, connecting("c2"), up2, up3},
			[]string{"up1", "up2", "up3"}},
		{"none up", []connectors.Connector{connecting("c1"), ended, connecting("c2")},
			[]string{"c1", "c2"}},
		{"none usable", []connectors.Connector{ended, noProber}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, target := range probeTargets(tt.conns) {
				got = append(got, target.server)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("targets = %q, want %q", got, tt.want)
			}
		})
	}
}
