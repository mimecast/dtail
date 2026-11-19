package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/sessionuser"
)

func TestNewForUserSelectsHandlerAndUsesExplicitDependencies(t *testing.T) {
	serverCfg := &config.ServerConfig{
		AuthKeyEnabled:      true,
		MaxCommandFrameSize: 321,
	}
	dependencies := Dependencies{
		ServerConfig: serverCfg,
		CatLimiter:   make(chan struct{}, 1),
		TailLimiter:  make(chan struct{}, 1),
		AuthKeyStore: authkey.New(time.Hour, 1),
		Loggers: HandlerLoggers{
			Diagnostics: logging.NopLogger{},
			Reader:      logging.NopLogger{},
		},
		Capabilities: []string{protocol.CapabilityQueryUpdateV1},
		Hostname:     "test-host",
	}

	tests := []struct {
		name     string
		userName string
		check    func(*testing.T, Handler)
	}{
		{
			name:     "health",
			userName: config.HealthUser,
			check: func(t *testing.T, handler Handler) {
				health, ok := handler.(*HealthHandler)
				if !ok {
					t.Fatalf("handler type = %T, want *HealthHandler", handler)
				}
				if health.maxCommandFrameSize != serverCfg.MaxCommandFrameSize {
					t.Fatalf("frame size = %d, want %d", health.maxCommandFrameSize, serverCfg.MaxCommandFrameSize)
				}
			},
		},
		{
			name:     "read session",
			userName: "alice",
			check: func(t *testing.T, handler Handler) {
				serverHandler, ok := handler.(*ServerHandler)
				if !ok {
					t.Fatalf("handler type = %T, want *ServerHandler", handler)
				}
				message := readServerMessage(t, serverHandler.serverMessages)
				want := protocol.HiddenCapabilitiesPrefix + protocol.CapabilityQueryUpdateV1
				if message != want {
					t.Fatalf("capability advertisement = %q, want %q", message, want)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := NewForUser(context.Background(), &sessionuser.User{Name: test.userName}, dependencies)
			if err != nil {
				t.Fatalf("NewForUser: %v", err)
			}
			test.check(t, handler)
		})
	}
}

func TestNewForUserRejectsMissingUser(t *testing.T) {
	_, err := NewForUser(context.Background(), nil, Dependencies{})
	if err == nil {
		t.Fatal("NewForUser accepted a nil user")
	}
}

func TestNewForUserRejectsNilContext(t *testing.T) {
	var nilContext context.Context
	_, err := NewForUser(nilContext, &sessionuser.User{Name: "alice"}, Dependencies{})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("NewForUser nil-context error = %v, want context error", err)
	}
}
