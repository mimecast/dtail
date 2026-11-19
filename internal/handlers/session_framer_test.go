package handlers

import (
	"context"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
)

func TestSessionFramerPreservesFragmentedAndAdjacentCommands(t *testing.T) {
	var got [][]string
	handler := newBaseHandler(context.Background(), baseHandlerConfig{})
	handler.handleCommandCb = func(ctx context.Context, _ lcontext.LContext,
		_ int, args []string, _ string) {
		got = append(got, append([]string(nil), args...))
		cancelCommandContext(ctx)
	}

	first := encodeTestCommand("probe first")
	second := encodeTestCommand("probe second")
	split := len(first) / 2
	if _, err := handler.Write([]byte(first[:split])); err != nil {
		t.Fatalf("write first command fragment: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fragment dispatched before delimiter: %v", got)
	}
	if _, err := handler.Write([]byte(first[split:] + second)); err != nil {
		t.Fatalf("write remaining and adjacent command frames: %v", err)
	}

	want := [][]string{{"probe", "first"}, {"probe", "second"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched commands = %v, want %v", got, want)
	}
}
