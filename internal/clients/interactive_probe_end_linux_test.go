//go:build linux

package clients

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/protocol"
)

// slowStopProbeHandler is a session handler whose probe blocks until its
// context ends and then takes a while to return, recording when it did.
type slowStopProbeHandler struct {
	*statusTestHandler
	open     chan struct{}
	started  chan struct{}
	returned atomic.Bool
}

func (h *slowStopProbeHandler) Done() <-chan struct{} { return h.open }

func (h *slowStopProbeHandler) Probe(ctx context.Context, _ protocol.ProbeRequest) (protocol.ProbeAnswer, error) {
	close(h.started)
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)
	h.returned.Store(true)
	return protocol.ProbeAnswer{}, ctx.Err()
}

// TestInteractiveControlWaitsForProber: an interactive session's Start
// returns only once its background prober stopped, also when a probe is
// still waiting for its answer at :quit, so no prober goroutine outlives the
// session.
func TestInteractiveControlWaitsForProber(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	c, _ := newSessionEndTestClient(t, func(ctx context.Context) { <-ctx.Done() })
	handler := &slowStopProbeHandler{
		statusTestHandler: &statusTestHandler{outcome: handlers.SessionOutcome{}},
		open:              make(chan struct{}),
		started:           make(chan struct{}),
	}
	c.connections[0].(*finalTestConnector).handler = handler
	c.sessionSpec.Files = []string{"/var/log/app.log"}
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Interactive query control enabled", done)
	select {
	case <-handler.started:
	case <-time.After(10 * time.Second):
		t.Fatal("prober sent no probe")
	}
	if _, err := master.WriteString(":quit\n"); err != nil {
		t.Fatalf("write :quit: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interactive session did not end")
	}
	if !handler.returned.Load() {
		t.Fatal("Start returned before the prober's probe returned")
	}
}
