package server

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	goUser "os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	serveruser "github.com/mimecast/dtail/internal/sessionuser"

	gossh "golang.org/x/crypto/ssh"
)

type testConnMetadata struct {
	user string
}

type testAddr string

func (m testConnMetadata) User() string          { return m.user }
func (m testConnMetadata) SessionID() []byte     { return nil }
func (m testConnMetadata) ClientVersion() []byte { return nil }
func (m testConnMetadata) ServerVersion() []byte { return nil }
func (m testConnMetadata) RemoteAddr() net.Addr  { return testAddr("remote") }
func (m testConnMetadata) LocalAddr() net.Addr   { return testAddr("local") }

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

func testPublicKey(t *testing.T, seedByte byte) gossh.PublicKey {
	t.Helper()

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey, err := gossh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatalf("build SSH public key: %v", err)
	}
	return publicKey
}

func TestAuthKeyStorePermissions(t *testing.T) {
	// Create an isolated store for this test — there is no package-level global.
	store := authkey.New(time.Hour, 5)

	key := testPublicKey(t, 21)

	if permissions := authKeyStorePermissions(store, "alice", key); permissions != nil {
		t.Fatalf("Expected nil permissions when no key is cached")
	}

	store.Add("alice", key)

	permissions := authKeyStorePermissions(store, "alice", key)
	if permissions == nil {
		t.Fatalf("Expected permissions when key is cached")
	}
	if fingerprint := permissions.Extensions["pubkey-fp"]; fingerprint != gossh.FingerprintSHA256(key) {
		t.Fatalf("Unexpected fingerprint: %s", fingerprint)
	}

	if permissions := authKeyStorePermissions(store, "bob", key); permissions != nil {
		t.Fatalf("Expected nil permissions for different user")
	}

	unknownKey := testPublicKey(t, 22)
	if permissions := authKeyStorePermissions(store, "alice", unknownKey); permissions != nil {
		t.Fatalf("Expected nil permissions for unknown key")
	}
}

func TestPublicKeyCallbackRejectsPasswordOnlyUsers(t *testing.T) {
	store := authkey.New(time.Hour, 5)
	key := testPublicKey(t, 23)

	for _, userName := range []string{config.HealthUser, config.ScheduleUser, config.ContinuousUser} {
		t.Run(userName, func(t *testing.T) {
			store.Add(userName, key)

			permissions, err := publicKeyCallback(testConnMetadata{user: userName}, key, true,
				t.TempDir(), "", store, logging.NopLogger{})
			if err == nil {
				t.Fatal("Expected public key authentication to be rejected")
			}
			if permissions != nil {
				t.Fatalf("Expected nil permissions, got %#v", permissions)
			}
			if !strings.Contains(err.Error(), "does not support public key authentication") {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}

func TestVerifyAuthorizedKeysSkipsMalformedLineWithoutParserProgress(t *testing.T) {
	user := testServerUser(t)
	firstKey := testPublicKey(t, 41)
	secondKey := testPublicKey(t, 42)

	firstLine := gossh.MarshalAuthorizedKey(firstKey)
	badLine := []byte("this is not an authorized key\n")
	secondLine := gossh.MarshalAuthorizedKey(secondKey)
	authorizedKeys := append(append(append([]byte{}, firstLine...), badLine...), secondLine...)

	parser := func(in []byte) (gossh.PublicKey, string, []string, []byte, error) {
		switch {
		case bytes.HasPrefix(in, firstLine):
			return firstKey, "", nil, in[len(firstLine):], nil
		case bytes.HasPrefix(in, badLine):
			return nil, "", nil, in, errors.New("parse error")
		case bytes.HasPrefix(in, secondLine):
			return secondKey, "", nil, in[len(secondLine):], nil
		default:
			return nil, "", nil, nil, errors.New("unexpected authorized_keys input")
		}
	}

	permissions, err := verifyAuthorizedKeysWithParser(user, authorizedKeys, secondKey,
		parser, logging.NopLogger{})
	if err != nil {
		t.Fatalf("verifyAuthorizedKeysWithParser failed: %v", err)
	}
	if permissions == nil {
		t.Fatalf("Expected permissions for key after malformed line")
	}
	if got := permissions.Extensions["pubkey-fp"]; got != gossh.FingerprintSHA256(secondKey) {
		t.Fatalf("Unexpected fingerprint: %s", got)
	}
}

func TestVerifyAuthorizedKeysSkipsMalformedLineWithRealParser(t *testing.T) {
	user := testServerUser(t)
	firstKey := testPublicKey(t, 43)
	secondKey := testPublicKey(t, 44)

	badLine := []byte("ssh-rsa !!!!\n")
	authorizedKeys := append(append(append([]byte{}, gossh.MarshalAuthorizedKey(firstKey)...),
		badLine...), gossh.MarshalAuthorizedKey(secondKey)...)

	sawParseError := false
	parseAuthorizedKeyLineByLine := func(in []byte) (gossh.PublicKey, string, []string, []byte, error) {
		line := in
		rest := []byte(nil)
		if lineEnd := bytes.IndexByte(in, '\n'); lineEnd >= 0 {
			line = in[:lineEnd+1]
			rest = in[lineEnd+1:]
		}

		authorizedPubKey, comment, options, _, err := gossh.ParseAuthorizedKey(line)
		if err != nil {
			if bytes.Equal(line, badLine) {
				sawParseError = true
			}
			return nil, "", nil, rest, err
		}

		return authorizedPubKey, comment, options, rest, nil
	}

	permissions, err := verifyAuthorizedKeysWithParser(user, authorizedKeys, secondKey,
		parseAuthorizedKeyLineByLine, logging.NopLogger{})
	if err != nil {
		t.Fatalf("verifyAuthorizedKeysWithParser failed: %v", err)
	}
	if permissions == nil {
		t.Fatalf("Expected permissions for key after malformed line")
	}
	if got := permissions.Extensions["pubkey-fp"]; got != gossh.FingerprintSHA256(secondKey) {
		t.Fatalf("Unexpected fingerprint: %s", got)
	}
	if !sawParseError {
		t.Fatalf("Expected malformed authorized_keys line to hit the real parser error path")
	}
}

func TestFindAuthorizedKeysPathUsesCacheDirWhenPresent(t *testing.T) {
	cwd := t.TempDir()
	cacheDir := "cache"
	user := testServerUser(t)
	wantPath := filepath.Join(cwd, cacheDir, "alice.authorized_keys")
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	want := gossh.MarshalAuthorizedKey(testPublicKey(t, 31))
	if err := os.WriteFile(wantPath, want, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	rootedPath, err := findAuthorizedKeysPath(user, cacheDir, cwd, func(string) (*goUser.User, error) {
		t.Fatalf("lookupUser should not be called when cached authorized_keys exists")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("findAuthorizedKeysPath failed: %v", err)
	}
	if rootedPath.Path() != wantPath {
		t.Fatalf("findAuthorizedKeysPath returned %q, want %q", rootedPath.Path(), wantPath)
	}

	got, err := rootedPath.ReadFile()
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile returned %q, want %q", got, want)
	}
}

func TestAuthorizedKeysPathForUserUsesConfiguredPath(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/ignored-environment-key")
	configuredPath := filepath.Join(t.TempDir(), "authorized_keys")
	path, err := authorizedKeysPathForUser(testServerUser(t), "", configuredPath)
	if err != nil {
		t.Fatalf("authorizedKeysPathForUser failed: %v", err)
	}
	if got, want := path.Path(), configuredPath; got != want {
		t.Fatalf("authorizedKeysPathForUser returned %q, want %q", got, want)
	}
}

func TestAuthorizedKeysPathForUserRejectsConfiguredDirectory(t *testing.T) {
	_, err := authorizedKeysPathForUser(testServerUser(t), "", string(filepath.Separator))
	if err == nil {
		t.Fatal("authorizedKeysPathForUser accepted a directory as the configured file path")
	}
}

func TestFindAuthorizedKeysPathIgnoresCwdForAbsoluteCacheDir(t *testing.T) {
	// An absolute cache dir (e.g. /var/run/dserver/cache on the BSD
	// packages) must resolve independently of the CWD dserver was
	// started from.
	cwd := filepath.Join(t.TempDir(), "unrelated-cwd")
	cacheDir := t.TempDir()
	user := testServerUser(t)
	wantPath := filepath.Join(cacheDir, "alice.authorized_keys")

	want := gossh.MarshalAuthorizedKey(testPublicKey(t, 33))
	if err := os.WriteFile(wantPath, want, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	rootedPath, err := findAuthorizedKeysPath(user, cacheDir, cwd, func(string) (*goUser.User, error) {
		t.Fatalf("lookupUser should not be called when cached authorized_keys exists")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("findAuthorizedKeysPath failed: %v", err)
	}
	if rootedPath.Path() != wantPath {
		t.Fatalf("findAuthorizedKeysPath returned %q, want %q", rootedPath.Path(), wantPath)
	}

	got, err := rootedPath.ReadFile()
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile returned %q, want %q", got, want)
	}
}

func TestFindAuthorizedKeysPathAbsoluteCacheDirMissingFileFallsBack(t *testing.T) {
	// An absolute cache dir without a per-user cache file must fall back
	// to ~/.ssh/authorized_keys instead of erroring out.
	cwd := t.TempDir()
	cacheDir := t.TempDir() // exists, but holds no alice.authorized_keys
	homeDir := t.TempDir()
	user := testServerUser(t)
	wantPath := filepath.Join(homeDir, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	want := gossh.MarshalAuthorizedKey(testPublicKey(t, 34))
	if err := os.WriteFile(wantPath, want, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	rootedPath, err := findAuthorizedKeysPath(user, cacheDir, cwd, func(name string) (*goUser.User, error) {
		return &goUser.User{Username: name, HomeDir: homeDir}, nil
	})
	if err != nil {
		t.Fatalf("findAuthorizedKeysPath failed: %v", err)
	}
	if rootedPath.Path() != wantPath {
		t.Fatalf("findAuthorizedKeysPath returned %q, want %q", rootedPath.Path(), wantPath)
	}

	got, err := rootedPath.ReadFile()
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile returned %q, want %q", got, want)
	}
}

func TestFindAuthorizedKeysPathFallsBackToHomeAuthorizedKeys(t *testing.T) {
	cwd := t.TempDir()
	homeDir := t.TempDir()
	user := testServerUser(t)
	wantPath := filepath.Join(homeDir, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	want := gossh.MarshalAuthorizedKey(testPublicKey(t, 32))
	if err := os.WriteFile(wantPath, want, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	rootedPath, err := findAuthorizedKeysPath(user, "cache", cwd, func(name string) (*goUser.User, error) {
		return &goUser.User{Username: name, HomeDir: homeDir}, nil
	})
	if err != nil {
		t.Fatalf("findAuthorizedKeysPath failed: %v", err)
	}
	if rootedPath.Path() != wantPath {
		t.Fatalf("findAuthorizedKeysPath returned %q, want %q", rootedPath.Path(), wantPath)
	}

	got, err := rootedPath.ReadFile()
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile returned %q, want %q", got, want)
	}
}

func TestFindAuthorizedKeysPathRejectsEscapingHomeSymlink(t *testing.T) {
	cwd := t.TempDir()
	homeDir := t.TempDir()
	user := testServerUser(t)
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	outsidePath := filepath.Join(homeDir, "outside_authorized_keys")
	if err := os.WriteFile(outsidePath, gossh.MarshalAuthorizedKey(testPublicKey(t, 33)), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "outside_authorized_keys"),
		filepath.Join(sshDir, "authorized_keys")); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	_, err := findAuthorizedKeysPath(user, "", cwd, func(name string) (*goUser.User, error) {
		return &goUser.User{Username: name, HomeDir: homeDir}, nil
	})
	if err == nil {
		t.Fatalf("findAuthorizedKeysPath succeeded for escaping authorized_keys symlink")
	}
}

func testServerUser(t *testing.T) *serveruser.User {
	t.Helper()

	user, err := serveruser.New("alice", "127.0.0.1:2222", nil, logging.NopLogger{})
	if err != nil {
		t.Fatalf("serveruser.New failed: %v", err)
	}
	return user
}
