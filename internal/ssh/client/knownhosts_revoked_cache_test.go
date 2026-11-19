package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh/knownhosts"
)

// A key marked @revoked is refused also in trust-all mode, and known_hosts
// is left as it is.
func TestWrapRefusesRevokedHostKeyInTrustAllMode(t *testing.T) {
	server, remote, key := batchHost(1)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	contents := "@revoked * " + strings.SplitN(knownhosts.Line([]string{server}, key), " ", 2)[1] + "\n"
	if err := os.WriteFile(knownHostsPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	callback := newTestKnownHostsCallback(t, knownHostsPath, true, sshClientTestLogger)
	err := callback.Wrap(context.Background())(server, remote, key)
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("host key check of a revoked key: %v, want a revoked key error", err)
	}
	got, readErr := os.ReadFile(knownHostsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != contents {
		t.Fatalf("known_hosts changed:\n%s\nwant:\n%s", got, contents)
	}
}

// The parsed known_hosts file is reused for further handshakes and parsed
// again once the file changed.
func TestKnownHostsCheckerCachesUntilTheFileChanges(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	writeHost := func(i int) {
		server, remote, key := batchHost(i)
		lines := knownhosts.Line([]string{server}, key) + "\n" +
			knownhosts.Line([]string{remote.String()}, key) + "\n"
		if err := os.WriteFile(knownHostsPath, []byte(strings.Repeat("# padding\n", i)+lines), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(callback *KnownHostsCallback, i int) error {
		checker, err := callback.knownHostsChecker()
		if err != nil {
			return err
		}
		server, remote, key := batchHost(i)
		return checker(server, remote, key)
	}

	writeHost(1)
	callback := newTestKnownHostsCallback(t, knownHostsPath, false, sshClientTestLogger)
	if err := check(callback, 1); err != nil {
		t.Fatalf("host 1 is not known: %v", err)
	}
	parsedFrom := callback.parsed.info
	if err := check(callback, 1); err != nil {
		t.Fatalf("host 1 is not known on the second check: %v", err)
	}
	if callback.parsed.info != parsedFrom {
		t.Fatal("the unchanged known_hosts file was parsed again")
	}

	// Another host and size: the file must be parsed again.
	writeHost(2)
	if err := check(callback, 2); err != nil {
		t.Fatalf("host 2 is not known after known_hosts changed: %v", err)
	}
	if err := check(callback, 1); err == nil {
		t.Fatal("host 1 is still known after it was removed from known_hosts")
	}
}
