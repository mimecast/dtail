package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

func TestDecodeGeneratedMessage(t *testing.T) {
	generation, message := decodeGeneratedMessage(encodeGeneratedMessage(7, "hello"))
	if generation != 7 {
		t.Fatalf("unexpected generation: %d", generation)
	}
	if message != "hello" {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestBaseHandlerReadDropsStaleServerMessage(t *testing.T) {
	handler := newGenerationTestHandler(2)
	handler.serverMessages <- encodeGeneratedMessage(1, "stale\n")
	handler.serverMessages <- encodeGeneratedMessage(2, "fresh\n")

	got := readHandlerOutput(t, handler)
	if strings.Contains(got, "stale") {
		t.Fatalf("unexpected stale output: %q", got)
	}
	if !strings.Contains(got, "fresh") {
		t.Fatalf("expected current output, got %q", got)
	}
}

func TestBaseHandlerReadDropsStaleMaprMessage(t *testing.T) {
	handler := newGenerationTestHandler(3)
	handler.maprMessages <- encodeGeneratedMessage(2, "old aggregate")
	handler.maprMessages <- encodeGeneratedMessage(3, "new aggregate")

	got := readHandlerOutput(t, handler)
	if strings.Contains(got, "old aggregate") {
		t.Fatalf("unexpected stale aggregate output: %q", got)
	}
	if !strings.Contains(got, "new aggregate") {
		t.Fatalf("expected current aggregate output, got %q", got)
	}
}

func TestBaseHandlerReadDropsStaleFlushError(t *testing.T) {
	handler := newGenerationTestHandler(2)
	handler.reportFlushError(1, errors.New("stale flush failure"))
	handler.reportFlushError(2, errors.New("current flush failure"))

	got := readHandlerOutput(t, handler)
	if strings.Contains(got, "stale flush failure") {
		t.Fatalf("unexpected stale flush error: %q", got)
	}
	if !strings.Contains(got, "current flush failure") {
		t.Fatalf("expected current flush error, got %q", got)
	}
}

func TestGeneratedMaprMessagesChannelCloseWaitsForForwarding(t *testing.T) {
	handler := &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{maprMessages: make(chan string)}),
	}

	generated, closeGenerated := handler.newGeneratedMaprMessagesChannel(7)
	generated <- "final aggregate"

	closed := make(chan struct{})
	go func() {
		closeGenerated()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("closeGenerated returned before mapreduce payload was forwarded")
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case message := <-handler.maprMessages:
		generation, payload := decodeGeneratedMessage(message)
		if generation != 7 {
			t.Fatalf("unexpected generation: %d", generation)
		}
		if payload != "final aggregate" {
			t.Fatalf("unexpected payload: %q", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for forwarded mapreduce payload")
	}

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closeGenerated to finish")
	}
}

func TestOutputManagerTryReadDropsStaleGeneration(t *testing.T) {

	manager := outputManager{mode: true}
	mustEnqueueOutput(t, &manager, 1, []byte("stale"))
	mustEnqueueOutput(t, &manager, 2, []byte("fresh"))

	buf := make([]byte, 32)
	n, handled := manager.tryRead(buf, &userserver.User{Name: "output-test"}, func(generation uint64) bool {
		return generation != 0 && generation != 2
	})
	if !handled {
		t.Fatalf("expected output read to be handled")
	}
	if got := string(buf[:n]); got != "fresh" {
		t.Fatalf("unexpected output output: %q", got)
	}
}

func newGenerationTestHandler(activeGeneration uint64) *baseHandler {
	return newBaseHandler(context.Background(), baseHandlerConfig{
		serverMessages: make(chan string, 2),
		maprMessages:   make(chan string, 2),
		hostname:       "testhost",
		activeGeneration: func() uint64 {
			return activeGeneration
		},
	})
}

func readHandlerOutput(t *testing.T, handler *baseHandler) string {
	t.Helper()

	buf := make([]byte, 256)
	n, err := handler.Read(buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return string(buf[:n])
}
