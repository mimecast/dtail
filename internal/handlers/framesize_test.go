package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// frameSizeTestServerHandler builds a minimal ServerHandler with a very small
// maxCommandFrameSize so tests can exercise the limit without allocating a
// full megabyte of data.
func frameSizeTestServerHandler(maxFrameBytes int) *ServerHandler {
	u := &userserver.User{Name: "frame-size-test-user"}
	h := &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{
			logger:              handlerTestLogger,
			serverMessages:      make(chan string, 8),
			maprMessages:        make(chan string, 4),
			user:                u,
			maxCommandFrameSize: maxFrameBytes,
		}),
		serverCfg: &config.ServerConfig{
			AuthKeyEnabled: true,
		},
		authKeyStore: authkey.New(time.Hour, 5),
	}
	// Use the real user-command handler so normal protocol paths work.
	h.handleCommandCb = h.handleUserCommand
	h.commands = h.newCommandRegistry()
	return h
}

// frameSizeTestHealthHandler builds a minimal HealthHandler with a small limit.
func frameSizeTestHealthHandler(maxFrameBytes int) *HealthHandler {
	u := &userserver.User{Name: "frame-size-health-test-user"}
	return &HealthHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{
			logger:              handlerTestLogger,
			serverMessages:      make(chan string, 8),
			maprMessages:        make(chan string, 4),
			user:                u,
			maxCommandFrameSize: maxFrameBytes,
		}),
	}
}

// TestWriteOversizeFrameClosesSession verifies that sending a byte stream that
// never emits a ';' delimiter and grows beyond maxCommandFrameSize causes Write
// to return io.ErrClosedPipe and shuts down the done channel so the SSH layer
// can tear down the connection.
func TestWriteOversizeFrameClosesSession(t *testing.T) {

	const limit = 64 // tiny limit to keep the test fast

	h := frameSizeTestServerHandler(limit)

	// Build a payload larger than the limit that contains no ';' delimiter.
	oversizeFrame := bytes.Repeat([]byte("x"), limit+1)

	_, err := h.Write(oversizeFrame)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe from Write on oversize frame, got %v", err)
	}

	// The done channel must be closed so callers can observe session termination.
	select {
	case <-h.done.Done():
		// expected
	default:
		t.Fatalf("expected handler done channel to be closed after oversize frame")
	}
	select {
	case <-h.commandRootCtx.Done():
		// expected
	default:
		t.Fatal("expected oversize frame shutdown to cancel command work")
	}
}

// TestWriteOversizeFrameHealthHandlerClosesSession exercises the same limit
// through the HealthHandler, which does not hold a ServerConfig and instead
// uses the default limit (or the one injected in tests via the struct field).
func TestWriteOversizeFrameHealthHandlerClosesSession(t *testing.T) {

	const limit = 32

	h := frameSizeTestHealthHandler(limit)

	oversizeFrame := bytes.Repeat([]byte("y"), limit+1)

	_, err := h.Write(oversizeFrame)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe from health handler Write on oversize frame, got %v", err)
	}

	select {
	case <-h.done.Done():
		// expected
	default:
		t.Fatalf("expected health handler done channel to be closed after oversize frame")
	}
	select {
	case <-h.commandRootCtx.Done():
		// expected
	default:
		t.Fatal("expected oversize health frame shutdown to cancel command work")
	}
}

// TestWriteFrameAtExactLimitIsAccepted verifies that a frame whose length equals
// the limit is still accepted (the guard fires only when the buffer *exceeds* the
// limit). This ensures the boundary condition is correct.
func TestWriteFrameAtExactLimitIsAccepted(t *testing.T) {

	const limit = 16

	h := frameSizeTestServerHandler(limit)

	// Frame of exactly `limit` bytes followed by a ';' delimiter — must succeed.
	frame := append(bytes.Repeat([]byte("z"), limit), ';')

	_, err := h.Write(frame)
	if err != nil {
		t.Fatalf("expected no error for frame at exact limit, got %v", err)
	}

	// The session must still be alive.
	select {
	case <-h.done.Done():
		t.Fatalf("expected handler to remain alive for frame at exact limit")
	default:
		// expected
	}
}

// TestWriteNormalFramesBelowLimitAreAccepted confirms that legitimate short
// frames (well below the limit) pass through without triggering the guard.
func TestWriteNormalFramesBelowLimitAreAccepted(t *testing.T) {

	const limit = 1024 // generously above any test payload

	h := frameSizeTestServerHandler(limit)

	// Several small frames; none should trigger the limit.
	for _, cmd := range []string{"health;", "health;", "health;"} {
		if _, err := h.Write([]byte(cmd)); err != nil {
			t.Fatalf("unexpected error writing normal frame %q: %v", cmd, err)
		}
	}

	select {
	case <-h.done.Done():
		t.Fatalf("expected handler to remain alive after small frames")
	default:
		// expected
	}
}

// TestWriteZeroLimitDisablesGuard confirms that setting maxCommandFrameSize to 0
// disables the limit entirely (the guard is not checked), so arbitrarily large
// frames are tolerated. This makes it easy to opt out of the check when needed.
func TestWriteZeroLimitDisablesGuard(t *testing.T) {

	const limit = 0 // disabled

	h := frameSizeTestServerHandler(limit)

	// Send 4 KiB without a delimiter — must not trigger the guard.
	largeFrame := bytes.Repeat([]byte("a"), 4096)
	if _, err := h.Write(largeFrame); err != nil {
		t.Fatalf("expected no error when limit is 0 (disabled), got %v", err)
	}

	select {
	case <-h.done.Done():
		t.Fatalf("expected handler to remain alive when limit is 0")
	default:
		// expected
	}
}

// TestDefaultMaxCommandFrameSizeMatchesServerConfig validates that the default
// value defined in the config package matches what newDefaultServerConfig
// populates into ServerConfig.MaxCommandFrameSize. This prevents them from
// drifting apart silently.
func TestDefaultMaxCommandFrameSizeMatchesServerConfig(t *testing.T) {
	cfg := config.ServerConfig{}
	// Retrieve through the exported helper that sets all defaults.
	defaultCfg := config.NewDefaultServerConfigForTest()
	cfg = *defaultCfg

	if cfg.MaxCommandFrameSize != config.DefaultMaxCommandFrameSize {
		t.Fatalf("ServerConfig.MaxCommandFrameSize default (%d) != config.DefaultMaxCommandFrameSize (%d)",
			cfg.MaxCommandFrameSize, config.DefaultMaxCommandFrameSize)
	}
}
