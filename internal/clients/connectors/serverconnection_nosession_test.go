package connectors

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

// closedPortAddress returns host:port of a local TCP port nothing listens
// on (a listener's port, closed again), so a dial is refused at once.
func closedPortAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return address
}

func newNoSessionConnection(t *testing.T, address string, handler handlers.Handler) *ServerConnection {
	t.Helper()
	conn, err := NewServerConnection(address, "user", nil, testHostKeyCallback{}, handler,
		nil, sessionspec.Spec{}, true, "", true,
		testSSHSettings{port: 2222, timeout: 2 * time.Second}, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewServerConnection: %v", err)
	}
	return conn
}

func requireHandlerDone(t *testing.T, handler handlers.Handler) {
	t.Helper()
	select {
	case <-handler.Done():
	default:
		t.Fatal("handler not done after Start returned without a session")
	}
}

// TestStartWithoutSessionShutsHandlerDown: a connection whose Start ended
// without a session (here a refused TCP dial) has its handler shut down, so
// whoever waits on the handler (a probe waiting for the session to come
// up, a :reload waiting for its acknowledgement) stops waiting at once
// instead of until its own timeout. The real ClientHandler's status still
// tells the connection error.
func TestStartWithoutSessionShutsHandlerDown(t *testing.T) {
	address := closedPortAddress(t)
	handler := handlers.NewClientHandler(address, nil)
	conn := newNoSessionConnection(t, address, handler)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn.Start(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1))

	requireHandlerDone(t, handler)
	if status := handler.Status(); status != 1 {
		t.Fatalf("status = %d, want 1 (connection error)", status)
	}
	probeCtx, probeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer probeCancel()
	if _, err := handler.Probe(probeCtx, protocol.ProbeRequest{ID: "p1", Files: []string{"/a.log"}}); !errors.Is(err, handlers.ErrProbeSessionEnded) {
		t.Fatalf("Probe error = %v, want %v", err, handlers.ErrProbeSessionEnded)
	}
}

// TestStartCanceledBeforeThrottleShutsHandlerDown: a Start that never got
// a throttle slot (its context ended first) ends without a session too.
func TestStartCanceledBeforeThrottleShutsHandlerDown(t *testing.T) {
	address := closedPortAddress(t)
	handler := handlers.NewClientHandler(address, nil)
	conn := newNoSessionConnection(t, address, handler)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A full throttle channel: only the ended context lets Start return.
	conn.Start(ctx, cancel, make(chan struct{}), make(chan struct{}, 1))

	requireHandlerDone(t, handler)
}
