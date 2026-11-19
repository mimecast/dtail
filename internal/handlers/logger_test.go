package handlers

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

var handlerTestLogger logging.NopLogger

type handlerRecordingLogger struct {
	logging.NopLogger
	events []string
}

func (l *handlerRecordingLogger) Info(args ...any) string {
	message := fmt.Sprint(args...)
	l.events = append(l.events, message)
	return message
}

func (l *handlerRecordingLogger) Debug(args ...any) string {
	message := fmt.Sprint(args...)
	l.events = append(l.events, message)
	return message
}

func TestNewServerHandlerPreservesInjectedRoles(t *testing.T) {
	diagnostics := &handlerRecordingLogger{}
	reader := &handlerRecordingLogger{}
	colorizer := brush.New(config.DefaultTermColors())
	var output bytes.Buffer
	handler, err := NewServerHandler(context.Background(),
		&userserver.User{Name: "logger-test"},
		Dependencies{
			CatLimiter:       make(chan struct{}, 1),
			TailLimiter:      make(chan struct{}, 1),
			ServerConfig:     &config.ServerConfig{AuthKeyEnabled: true},
			AuthKeyStore:     authkey.New(time.Hour, 1),
			ServerlessOutput: &output,
			Loggers:          HandlerLoggers{Diagnostics: diagnostics, Reader: reader},
			Colorizer:        colorizer,
		},
	)
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	if handler.Logger() != diagnostics {
		t.Fatal("diagnostic logger was not preserved")
	}
	if handler.ReaderLogger() != reader {
		t.Fatal("reader logger was not preserved")
	}
	if handler.ServerlessOutput() != &output {
		t.Fatal("serverless output writer was not preserved")
	}
	if handler.colorizer != colorizer {
		t.Fatal("terminal colorizer was not preserved")
	}
	handler.Logger().Info("diagnostic-event")
	handler.ReaderLogger().Info("reader-event")
	if len(diagnostics.events) != 2 || diagnostics.events[1] != "diagnostic-event" {
		t.Fatalf("diagnostic events = %q, want constructor and diagnostic events", diagnostics.events)
	}
	if len(reader.events) != 1 || reader.events[0] != "reader-event" {
		t.Fatalf("reader events = %q, want only reader event", reader.events)
	}
}

func TestNewHealthHandlerPreservesInjectedLogger(t *testing.T) {
	diagnostics := &handlerRecordingLogger{}
	handler, err := NewHealthHandler(context.Background(),
		&userserver.User{Name: "health-logger-test"}, config.DefaultMaxCommandFrameSize,
		"test-host", diagnostics)
	if err != nil {
		t.Fatalf("NewHealthHandler: %v", err)
	}
	if handler.Logger() != diagnostics {
		t.Fatal("diagnostic logger was not preserved")
	}
	handler.Logger().Info("health-event")
	if len(diagnostics.events) != 2 || diagnostics.events[1] != "health-event" {
		t.Fatalf("health diagnostic events = %q, want constructor and health events", diagnostics.events)
	}
}
