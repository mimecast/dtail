package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"
)

// writeKnownHost writes a known_hosts file knowing server under the key of
// host number keyOf.
func writeKnownHost(t *testing.T, path, server string, keyOf int) string {
	t.Helper()
	_, _, key := batchHost(keyOf)
	contents := knownhosts.Line([]string{server}, key) + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return contents
}

// A host whose key differs from the one in known_hosts is refused, also in
// trust-all mode, and its entry stays as it is.
func TestWrapRefusesChangedHostKeyByDefault(t *testing.T) {
	server, remote, key := batchHost(1)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	contents := writeKnownHost(t, knownHostsPath, server, 2)

	for _, trustAll := range []bool{false, true} {
		callback := newTestKnownHostsCallback(t, knownHostsPath, trustAll, sshClientTestLogger)
		err := callback.Wrap(context.Background())(server, remote, key)
		if err == nil || !strings.Contains(err.Error(), "HOST KEY CHANGED") ||
			!strings.Contains(err.Error(), "--trustChangedHostKeys") {
			t.Fatalf("trustAll=%v: host key check = %v, want a changed host key refusal", trustAll, err)
		}
		if !callback.Untrusted(server) {
			t.Fatalf("trustAll=%v: the refused host is not marked untrusted", trustAll)
		}
		got, readErr := os.ReadFile(knownHostsPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != contents {
			t.Fatalf("trustAll=%v: known_hosts changed:\n%s", trustAll, got)
		}
	}
}

// With --trustChangedHostKeys, trust-all mode accepts the changed key and
// replaces the entry.
func TestWrapReplacesChangedHostKeyWhenAllowed(t *testing.T) {
	server, remote, key := batchHost(1)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	writeKnownHost(t, knownHostsPath, server, 2)

	callback := newTestKnownHostsCallback(t, knownHostsPath, true, sshClientTestLogger)
	callback.TrustChangedKeys(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go callback.PromptAddHosts(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- callback.Wrap(ctx)(server, remote, key) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("host key check with --trustChangedHostKeys failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host key check did not return")
	}

	got, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := knownhosts.Line([]string{server}, key); !strings.Contains(string(got), want) {
		t.Fatalf("known_hosts does not hold the new key:\n%s", got)
	}
	_, _, oldKey := batchHost(2)
	if old := knownhosts.Line([]string{server}, oldKey); strings.Contains(string(got), old) {
		t.Fatalf("known_hosts still holds the old key:\n%s", got)
	}
}
