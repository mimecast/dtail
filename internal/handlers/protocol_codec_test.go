package handlers

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestHandleProtocolVersionUsesSemanticCompatComparison(t *testing.T) {
	codec := newProtocolCodec(nil, handlerTestLogger)

	args, argc, add, client, err := codec.handleProtocolVersion([]string{"protocol", "4", "tail", "payload"})
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
	if client != protocol4Client {
		t.Fatalf("protocol 4 reported as client kind %d, want protocol4Client", client)
	}
	if !strings.Contains(err.Error(), "please update DTail client") {
		t.Fatalf("expected client update guidance, got %q", err)
	}
	if !strings.Contains(err.Error(), protocol.ProtocolCompat) {
		t.Fatalf("expected error to mention server protocol version, got %q", err)
	}
}

// TestHandleProtocolVersionClientKind: the kind of a mismatched client
// (mismatchedClient) is legacyClient for protocol major version 3 or lower,
// unparsable versions and frames without a protocol prefix (v1.x clients
// send "%s;"), protocol4Client for major version 4 and otherVersionClient
// for every other version, independently of add, which keeps its newline
// only for versions whose major parses as 3 or lower (a v1.x client never
// got a newline).
func TestHandleProtocolVersionClientKind(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		add    string
		client mismatchedClient
	}{
		{"protocol 3", []string{"protocol", "3.9", "base64", "x"}, "\n", legacyClient},
		{"protocol 4", []string{"protocol", "4", "base64", "x"}, "", protocol4Client},
		{"protocol 4.1", []string{"protocol", "4.1", "base64", "x"}, "", protocol4Client},
		{"protocol 5.1", []string{"protocol", "5.1", "base64", "x"}, "", otherVersionClient},
		{"protocol 6", []string{"protocol", "6", "base64", "x"}, "", otherVersionClient},
		{"unparsable version", []string{"protocol", "x.y", "base64", "x"}, "\n", legacyClient},
		{"no protocol prefix", []string{"cat", "test.log", "."}, "", legacyClient},
		{"prefix only", []string{"protocol", "4"}, "", legacyClient},
	}
	codec := newProtocolCodec(nil, handlerTestLogger)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, add, client, err := codec.handleProtocolVersion(tc.args)
			if err == nil {
				t.Fatal("expected protocol mismatch error")
			}
			if add != tc.add {
				t.Fatalf("message separator %q, want %q", add, tc.add)
			}
			if client != tc.client {
				t.Fatalf("client kind %d, want %d", client, tc.client)
			}
		})
	}
}

// TestHandleProtocolVersionCurrentVersion: a frame of this server's protocol
// version is no mismatch (noMismatch); the prefix is stripped.
func TestHandleProtocolVersionCurrentVersion(t *testing.T) {
	codec := newProtocolCodec(nil, handlerTestLogger)
	args, argc, add, client, err := codec.handleProtocolVersion(
		[]string{"protocol", protocol.ProtocolCompat, "base64", "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if argc != 2 || len(args) != 2 || args[0] != "base64" || add != "" {
		t.Fatalf("unexpected result: args %#v argc %d add %q", args, argc, add)
	}
	if client != noMismatch {
		t.Fatalf("client kind %d, want noMismatch", client)
	}
}
