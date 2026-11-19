package handlers

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestHandleProtocolVersionUsesSemanticCompatComparison(t *testing.T) {
	codec := newProtocolCodec(nil, handlerTestLogger)

	args, argc, add, legacy, err := codec.handleProtocolVersion([]string{"protocol", "4", "tail", "payload"})
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
	if legacy {
		t.Fatal("protocol 4 reported as a legacy client")
	}
	if !strings.Contains(err.Error(), "please update DTail client") {
		t.Fatalf("expected client update guidance, got %q", err)
	}
	if !strings.Contains(err.Error(), protocol.ProtocolCompat) {
		t.Fatalf("expected error to mention server protocol version, got %q", err)
	}
}

// TestHandleProtocolVersionLegacy: legacy is reported for clients of
// protocol major version 3 or lower, unparsable versions and frames without
// a protocol prefix (v1.x clients send "%s;"), independently of add, which
// keeps its newline only for versions whose major parses as 3 or lower (a
// v1.x client never got a newline).
func TestHandleProtocolVersionLegacy(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		add    string
		legacy bool
	}{
		{"protocol 3", []string{"protocol", "3.9", "base64", "x"}, "\n", true},
		{"protocol 4", []string{"protocol", "4", "base64", "x"}, "", false},
		{"protocol 5.1", []string{"protocol", "5.1", "base64", "x"}, "", false},
		{"unparsable version", []string{"protocol", "x.y", "base64", "x"}, "\n", true},
		{"no protocol prefix", []string{"cat", "test.log", "."}, "", true},
		{"prefix only", []string{"protocol", "4"}, "", true},
	}
	codec := newProtocolCodec(nil, handlerTestLogger)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, add, legacy, err := codec.handleProtocolVersion(tc.args)
			if err == nil {
				t.Fatal("expected protocol mismatch error")
			}
			if add != tc.add {
				t.Fatalf("message separator %q, want %q", add, tc.add)
			}
			if legacy != tc.legacy {
				t.Fatalf("legacy %v, want %v", legacy, tc.legacy)
			}
		})
	}
}
