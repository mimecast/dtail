package connectors

import (
	"slices"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestSendCommandStreamFramesReadBatchWithoutCapabilityWait(t *testing.T) {
	handler := &mockHandler{}
	commands := []string{"map query", "cat first.log .", "cat second.log ."}

	if err := sendCommandStream(handler, commands); err != nil {
		t.Fatalf("send command stream: %v", err)
	}
	want := []string{
		protocol.InputBatchBeginCommand,
		"map query",
		"cat first.log .",
		"cat second.log .",
		protocol.InputBatchCompleteCommand,
	}
	if !slices.Equal(handler.commands, want) {
		t.Fatalf("sent commands = %q, want %q", handler.commands, want)
	}
}

func TestSendCommandStreamFramesTimeoutPrefixedReadBatch(t *testing.T) {
	handler := &mockHandler{}
	commands := []string{"timeout 10 grep:plain=true error app.log"}

	if err := sendCommandStream(handler, commands); err != nil {
		t.Fatalf("send command stream: %v", err)
	}
	want := []string{
		protocol.InputBatchBeginCommand,
		commands[0],
		protocol.InputBatchCompleteCommand,
	}
	if !slices.Equal(handler.commands, want) {
		t.Fatalf("sent commands = %q, want %q", handler.commands, want)
	}
}

func TestSendCommandStreamLeavesHealthCommandUnframed(t *testing.T) {
	handler := &mockHandler{}
	commands := []string{"health"}

	if err := sendCommandStream(handler, commands); err != nil {
		t.Fatalf("send command stream: %v", err)
	}
	if !slices.Equal(handler.commands, commands) {
		t.Fatalf("sent commands = %q, want %q", handler.commands, commands)
	}
}
