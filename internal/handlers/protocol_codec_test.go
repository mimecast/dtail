package handlers

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestHandleProtocolVersionUsesSemanticCompatComparison(t *testing.T) {
	codec := newProtocolCodec(nil, handlerTestLogger)

	args, argc, add, err := codec.handleProtocolVersion([]string{"protocol", "4", "tail", "payload"})
	if err == nil {
		t.Fatal("expected protocol mismatch error")
	}
	if argc != 4 {
		t.Fatalf("unexpected argc: got %d want 4", argc)
	}
	if len(args) != 4 || args[0] != "protocol" || args[1] != "4" {
		t.Fatalf("unexpected args returned: %#v", args)
	}
	if add != "" {
		t.Fatalf("unexpected message separator: %q", add)
	}
	if !strings.Contains(err.Error(), "please update DTail client") {
		t.Fatalf("expected client update guidance, got %q", err)
	}
	if !strings.Contains(err.Error(), protocol.ProtocolCompat) {
		t.Fatalf("expected error to mention server protocol version, got %q", err)
	}
}
