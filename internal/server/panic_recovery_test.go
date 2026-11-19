package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

type panicRecordingLogger struct {
	logging.NopLogger
	mu     sync.Mutex
	errors []string
}

func (l *panicRecordingLogger) Error(args ...any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	message := fmt.Sprint(args...)
	l.errors = append(l.errors, message)
	return message
}

func (l *panicRecordingLogger) joinedErrors() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.errors, "\n")
}

func TestRecoverGoroutinePanicContainsFailure(t *testing.T) {
	logger := &panicRecordingLogger{}
	server := &Server{logger: logger}
	cleanupCalled := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer server.recoverGoroutinePanic("test session", "test-user", func() {
			close(cleanupCalled)
		})
		panic("boom")
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panicking goroutine did not recover")
	}
	select {
	case <-cleanupCalled:
	default:
		t.Fatal("panic cleanup was not called")
	}

	logOutput := logger.joinedErrors()
	for _, want := range []string{"Recovered panic", "test session", "test-user", "boom", "goroutine"} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("panic log %q does not contain %q", logOutput, want)
		}
	}
}

func TestRecoverGoroutinePanicContainsCleanupFailure(t *testing.T) {
	logger := &panicRecordingLogger{}
	server := &Server{logger: logger}
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer server.recoverGoroutinePanic("test cleanup", "test-user", func() {
			panic("cleanup failed")
		})
		panic("work failed")
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup panic escaped or blocked recovery")
	}

	logOutput := logger.joinedErrors()
	for _, want := range []string{
		"Recovered panic", "work failed", "Recovered panic while cleaning up goroutine",
		"cleanup failed", "test cleanup", "test-user", "goroutine",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("panic log %q does not contain %q", logOutput, want)
		}
	}
}
