package connectors

import (
	"slices"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestSendLegacyCommandsFramesReadBatchWithoutCapabilityWait(t *testing.T) {
	handler := &mockHandler{}
	commands := []string{"map query", "cat first.log .", "cat second.log ."}

	if err := sendLegacyCommands(handler, commands); err != nil {
		t.Fatalf("send legacy commands: %v", err)
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

func TestSendLegacyCommandsFramesTimeoutPrefixedReadBatch(t *testing.T) {
	handler := &mockHandler{}
	commands := []string{"timeout 10 grep:plain=true error app.log"}

	if err := sendLegacyCommands(handler, commands); err != nil {
		t.Fatalf("send legacy commands: %v", err)
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

func TestSendLegacyCommandsLeavesHealthCommandUnframed(t *testing.T) {
	handler := &mockHandler{}
	commands := []string{"health"}

	if err := sendLegacyCommands(handler, commands); err != nil {
		t.Fatalf("send legacy commands: %v", err)
	}
	if !slices.Equal(handler.commands, commands) {
		t.Fatalf("sent commands = %q, want %q", handler.commands, commands)
	}
}
