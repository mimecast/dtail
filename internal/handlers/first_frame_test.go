package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// TestCapabilitiesFrameIsFirstDespiteQueuedOutput pins that the session's
// first frame is the capabilities frame even when payload and a server
// message are queued before the output goroutine reads: protocol 5 clients
// reject a session whose first frame is anything else. Queued as an
// ordinary server message, the capabilities could be overtaken by payload,
// which reaches the output through another queue (seen as a rare
// "not a protocol 5 server" failure of dgrep under load).
func TestCapabilitiesFrameIsFirstDespiteQueuedOutput(t *testing.T) {
	handler, err := NewServerHandler(context.Background(),
		&userserver.User{Name: "first-frame-user"},
		Dependencies{
			CatLimiter:   make(chan struct{}, 1),
			TailLimiter:  make(chan struct{}, 1),
			ServerConfig: &config.ServerConfig{AuthKeyEnabled: true},
			AuthKeyStore: authkey.New(time.Hour, 5),
			Loggers:      HandlerLoggers{Diagnostics: handlerTestLogger, Reader: handlerTestLogger},
			Capabilities: []string{protocol.CapabilityJournalV1},
			Hostname:     "test-host",
		},
	)
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	if !handler.output.enable() {
		t.Fatal("expected output enable to switch output mode on")
	}
	payload := []byte("payload line\n")
	mustEnqueueOutput(t, &handler.output, 0, payload)
	handler.serverMessages <- "a server message"

	var want bytes.Buffer
	sessionFraming.EncodeControl(&want, protocol.HiddenCapabilitiesPrefix+protocol.CapabilityJournalV1)
	got := readExactly(t, handler.baseHandler, 16, want.Len())
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("first frame = %q, want the capabilities frame %q", got, want.Bytes())
	}
}
