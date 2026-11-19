package clients

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

// TestSessionStartCommitsOverSlowLink starts an interactive session that
// cats a large file over a slow link: the in-process server's output is
// read 64 KiB every 150 ms (about 430 KiB/s), framed through the client
// handler as from a remote dserver. The client waits 2 s for the SESSION
// START acknowledgement (defaultSessionAckTimeout) and does not count a
// slow link as a paused output, so the acknowledgement must not queue
// behind the session output buffer (2 MiB and more, about 5 s here):
// dserver sends it ahead of the workload's payload (sessionAckGate).
// Before, the start failed with an acknowledgement timeout.
func TestSessionStartCommitsOverSlowLink(t *testing.T) {
	file := writeProbeTestLog(t, 60000)
	cfg := config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "slowlinkhost"},
	}
	runtime := newClientRuntimeBoundary(cfg, NewLoggerDependencies(clientlog.NopLogger{},
		logging.NopLogger{}, logging.NopLogger{}))
	handler := handlers.NewClientHandler("local(serverless)", clientlog.NopLogger{})
	spec := SessionSpec{Mode: omode.CatClient, Files: []string{file}}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatal(err)
	}
	slow := &atomic.Bool{}
	slow.Store(true)
	connector := connectors.NewServerless("slowlink-user", handler, commands, spec, true,
		slowLinkFactory{framed: framedServerlessFactory{runtime: runtime}, slow: slow}, logging.NopLogger{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	go func() {
		defer close(started)
		connector.Start(ctx, cancel, nil, nil)
	}()
	// The link gets fast for the teardown, which drains the output.
	defer func() { slow.Store(false); cancel(); <-started }()

	deadline := time.After(10 * time.Second)
	for {
		if _, generation, committed := connector.CommittedSession(); committed {
			if generation != 1 {
				t.Fatalf("committed generation = %d, want 1", generation)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the session start failed over the slow link: status %d", handler.Status())
		case <-deadline:
			t.Fatal("the session start was not committed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// slowLinkFactory builds the framed in-process server handler of
// TestSessionStartCommitsOverSlowLink behind a slow link.
type slowLinkFactory struct {
	framed framedServerlessFactory
	slow   *atomic.Bool
}

func (f slowLinkFactory) NewServerlessHandler(ctx context.Context,
	userName string) (sessionHandlers.Handler, error) {
	handler, err := f.framed.NewServerlessHandler(ctx, userName)
	if err != nil {
		return nil, err
	}
	return slowLinkHandler{Handler: handler, slow: f.slow}, nil
}

// slowLinkHandler delays every Read of the server's output by 150 ms while
// slow is set; the serverless connector reads 64 KiB at a time.
type slowLinkHandler struct {
	sessionHandlers.Handler
	slow *atomic.Bool
}

func (h slowLinkHandler) Read(p []byte) (int, error) {
	if h.slow.Load() {
		time.Sleep(150 * time.Millisecond)
	}
	return h.Handler.Read(p)
}

// AttachOutputReader forwards the reader ownership the connector declares
// (connectors outputReaderAttacher), which the embedded interface hides.
func (h slowLinkHandler) AttachOutputReader() {
	if attacher, ok := h.Handler.(interface{ AttachOutputReader() }); ok {
		attacher.AttachOutputReader()
	}
}
