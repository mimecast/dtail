package client

import (
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/logging"

	gossh "golang.org/x/crypto/ssh"
)

var sshClientTestLogger logging.NopLogger

// testCloser is a sentinel io.Closer used by tests to assert that callers
// release ssh-agent connections returned by the mocked agentSigners hook.
type testCloser struct {
	closed int
}

func (c *testCloser) Close() error {
	c.closed++
	return nil
}

type mockPublicKey struct {
	id string
}

func (k *mockPublicKey) Type() string {
	return "ssh-rsa"
}

func (k *mockPublicKey) Marshal() []byte {
	return []byte(k.id)
}

func (k *mockPublicKey) Verify(_ []byte, _ *gossh.Signature) error {
	return nil
}

type mockSigner struct {
	key gossh.PublicKey
}

func newMockSigner(id string) gossh.Signer {
	return &mockSigner{key: &mockPublicKey{id: id}}
}

func (s *mockSigner) PublicKey() gossh.PublicKey {
	return s.key
}

func (s *mockSigner) Sign(_ io.Reader, _ []byte) (*gossh.Signature, error) {
	return &gossh.Signature{
		Format: "ssh-rsa",
		Blob:   []byte("sig"),
	}, nil
}

func TestInitSSHAuthMethodsUsesConfiguredPaths(t *testing.T) {
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	privateKeyPath := "/configured/id_rsa"
	t.Setenv("HOME", "/tmp/dtail-auth-config-home")
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/ignored-environment-key")

	originalPrivateKeySigner := privateKeySigner
	originalAgentSigners := agentSigners
	t.Cleanup(func() {
		privateKeySigner = originalPrivateKeySigner
		agentSigners = originalAgentSigners
	})

	var attemptedPaths []string
	privateKeySigner = func(path string) (gossh.Signer, error) {
		attemptedPaths = append(attemptedPaths, path)
		if path == privateKeyPath {
			return newMockSigner("configured"), nil
		}
		return nil, fmt.Errorf("missing private key: %s", path)
	}
	agentSigners = func(int, logging.Logger) ([]gossh.Signer, io.Closer, error) {
		return nil, noAuthCloser, nil
	}

	methods, callback, closer, err := InitSSHAuthMethods(nil, nil, AuthMethodConfig{
		TrustAllHosts:  true,
		KnownHostsPath: knownHostsPath,
		PrivateKeyPath: privateKeyPath,
		AgentKeyIndex:  -1,
	}, sshClientTestLogger, sshClientTestLogger)
	if err != nil {
		t.Fatalf("InitSSHAuthMethods: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("auth method count = %d, want 1", len(methods))
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close auth resources: %v", err)
	}
	knownHostsCallback, ok := callback.(*KnownHostsCallback)
	if !ok {
		t.Fatalf("callback type = %T, want *KnownHostsCallback", callback)
	}
	if got := knownHostsCallback.knownHostsPath; got != knownHostsPath {
		t.Fatalf("known hosts path = %q, want %q", got, knownHostsPath)
	}
	for _, path := range attemptedPaths {
		if path == "/tmp/ignored-environment-key" {
			t.Fatalf("SSH auth layer selected environment path %q", path)
		}
	}
}

func TestInitSSHAuthMethodsRejectsConfiguredKnownHostsDirectory(t *testing.T) {
	_, _, _, err := InitSSHAuthMethods(nil, nil, AuthMethodConfig{
		KnownHostsPath: string(filepath.Separator),
	}, sshClientTestLogger, sshClientTestLogger)
	if err == nil {
		t.Fatal("InitSSHAuthMethods accepted a directory as known_hosts path")
	}
}

func TestCollectKnownHostsAuthMethodsOrder(t *testing.T) {
	homeDir := "/tmp/dtail-auth-order"
	t.Setenv("HOME", homeDir)

	originalPrivateKeySigner := privateKeySigner
	originalAgentSigners := agentSigners
	t.Cleanup(func() {
		privateKeySigner = originalPrivateKeySigner
		agentSigners = originalAgentSigners
	})

	var callOrder []string
	successfulPrivateKeys := map[string]gossh.Signer{
		"/custom/id_fast":        newMockSigner("custom"),
		homeDir + "/.ssh/id_rsa": newMockSigner("default-rsa"),
		homeDir + "/.ssh/id_dsa": newMockSigner("default-dsa"),
	}

	privateKeySigner = func(path string) (gossh.Signer, error) {
		callOrder = append(callOrder, "private:"+path)
		signer, found := successfulPrivateKeys[path]
		if !found {
			return nil, fmt.Errorf("missing private key: %s", path)
		}
		return signer, nil
	}
	agentCloser := &testCloser{}
	agentSigners = func(keyIndex int, _ logging.Logger) ([]gossh.Signer, io.Closer, error) {
		callOrder = append(callOrder, fmt.Sprintf("agent:%d", keyIndex))
		return []gossh.Signer{newMockSigner("agent")}, agentCloser, nil
	}

	methods, closer, err := collectKnownHostsAuthMethods("/custom/id_fast", nil, 7, sshClientTestLogger)
	if err != nil {
		t.Fatalf("collectKnownHostsAuthMethods: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("Expected 1 auth method, got %d", len(methods))
	}
	if closer == nil {
		t.Fatalf("Expected non-nil agent closer from collectKnownHostsAuthMethods")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("agent closer returned error: %v", err)
	}
	if agentCloser.closed < 1 {
		t.Fatalf("Expected caller to be able to close agent conn; closed=%d", agentCloser.closed)
	}

	callOrder = nil
	signers, sCloser := collectKnownHostsSigners("/custom/id_fast", nil, 7, sshClientTestLogger)
	if len(signers) != 4 {
		t.Fatalf("Expected 4 signers, got %d", len(signers))
	}
	if sCloser == nil {
		t.Fatalf("Expected non-nil agent closer from collectKnownHostsSigners")
	}
	_ = sCloser.Close()

	expectedOrder := []string{
		"private:/custom/id_fast",
		"agent:7",
		"private:/tmp/dtail-auth-order/.ssh/id_rsa",
		"private:/tmp/dtail-auth-order/.ssh/id_dsa",
		"private:/tmp/dtail-auth-order/.ssh/id_ecdsa",
		"private:/tmp/dtail-auth-order/.ssh/id_ed25519",
	}
	if !reflect.DeepEqual(callOrder, expectedOrder) {
		t.Fatalf("Unexpected auth method call order.\nexpected: %v\ngot:      %v", expectedOrder, callOrder)
	}
}

func TestCollectKnownHostsAuthMethodsSkipsDuplicateDefaultPath(t *testing.T) {
	homeDir := "/tmp/dtail-auth-dedupe"
	t.Setenv("HOME", homeDir)

	originalPrivateKeySigner := privateKeySigner
	originalAgentSigners := agentSigners
	t.Cleanup(func() {
		privateKeySigner = originalPrivateKeySigner
		agentSigners = originalAgentSigners
	})

	sharedSigner := newMockSigner("shared")
	var callOrder []string
	privateKeySigner = func(path string) (gossh.Signer, error) {
		callOrder = append(callOrder, "private:"+path)
		if path == homeDir+"/.ssh/id_rsa" {
			return sharedSigner, nil
		}
		return nil, fmt.Errorf("missing private key: %s", path)
	}
	agentCloser := &testCloser{}
	agentSigners = func(keyIndex int, _ logging.Logger) ([]gossh.Signer, io.Closer, error) {
		callOrder = append(callOrder, fmt.Sprintf("agent:%d", keyIndex))
		return []gossh.Signer{sharedSigner}, agentCloser, nil
	}

	methods, closer, err := collectKnownHostsAuthMethods(homeDir+"/.ssh/id_rsa", nil, 2, sshClientTestLogger)
	if err != nil {
		t.Fatalf("collectKnownHostsAuthMethods: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("Expected 1 auth method, got %d", len(methods))
	}
	if closer == nil {
		t.Fatalf("Expected non-nil agent closer from collectKnownHostsAuthMethods")
	}
	_ = closer.Close()

	callOrder = nil
	signers, sCloser := collectKnownHostsSigners(homeDir+"/.ssh/id_rsa", nil, 2, sshClientTestLogger)
	if len(signers) != 1 {
		t.Fatalf("Expected duplicate keys to collapse to 1 signer, got %d", len(signers))
	}
	if sCloser == nil {
		t.Fatalf("Expected non-nil agent closer from collectKnownHostsSigners")
	}
	_ = sCloser.Close()

	expectedOrder := []string{
		"private:/tmp/dtail-auth-dedupe/.ssh/id_rsa",
		"agent:2",
		"private:/tmp/dtail-auth-dedupe/.ssh/id_dsa",
		"private:/tmp/dtail-auth-dedupe/.ssh/id_ecdsa",
		"private:/tmp/dtail-auth-dedupe/.ssh/id_ed25519",
	}
	if !reflect.DeepEqual(callOrder, expectedOrder) {
		t.Fatalf("Unexpected auth method call order.\nexpected: %v\ngot:      %v", expectedOrder, callOrder)
	}
}

func TestCollectKnownHostsAuthMethodsReturnsErrorAndClosesAgentWithoutKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	originalPrivateKeySigner := privateKeySigner
	originalAgentSigners := agentSigners
	t.Cleanup(func() {
		privateKeySigner = originalPrivateKeySigner
		agentSigners = originalAgentSigners
	})

	privateKeySigner = func(string) (gossh.Signer, error) {
		return nil, fmt.Errorf("missing key")
	}
	agentCloser := &testCloser{}
	agentSigners = func(int, logging.Logger) ([]gossh.Signer, io.Closer, error) {
		return nil, agentCloser, nil
	}

	methods, closer, err := collectKnownHostsAuthMethods("/missing/explicit-key", nil, 0, sshClientTestLogger)
	if err == nil {
		t.Fatal("collectKnownHostsAuthMethods succeeded without any usable key")
	}
	if methods != nil || closer != nil {
		t.Fatalf("failure returned methods=%v closer=%v, want nil resources", methods, closer)
	}
	if agentCloser.closed != 1 {
		t.Fatalf("agent closer calls = %d, want 1", agentCloser.closed)
	}
}

func TestCollectKnownHostsSignersIncludesConfiguredFallbackForExplicitKey(t *testing.T) {
	homeDir := "/tmp/dtail-auth-integration-fallback"
	suiteKeyPath := "/tmp/dtail-suite-auth/id_rsa"
	explicitKeyPath := "/tmp/dtail-explicit-auth/id_rsa"
	t.Setenv("HOME", homeDir)
	// Environment state must not select SSH trust or key paths in this layer.
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "yes")
	t.Setenv("DTAIL_AUTH_KEY_PATH", "/tmp/ignored-environment-key")

	originalPrivateKeySigner := privateKeySigner
	originalAgentSigners := agentSigners
	t.Cleanup(func() {
		privateKeySigner = originalPrivateKeySigner
		agentSigners = originalAgentSigners
	})

	var callOrder []string
	privateKeySigner = func(path string) (gossh.Signer, error) {
		callOrder = append(callOrder, "private:"+path)
		switch path {
		case explicitKeyPath:
			return newMockSigner("explicit"), nil
		case suiteKeyPath:
			return newMockSigner("suite"), nil
		default:
			return nil, fmt.Errorf("missing private key: %s", path)
		}
	}
	agentSigners = func(keyIndex int, _ logging.Logger) ([]gossh.Signer, io.Closer, error) {
		callOrder = append(callOrder, fmt.Sprintf("agent:%d", keyIndex))
		return nil, noAuthCloser, nil
	}

	signers, closer := collectKnownHostsSigners(explicitKeyPath, []string{suiteKeyPath}, 4, sshClientTestLogger)
	if len(signers) != 2 {
		t.Fatalf("Expected explicit and integration fallback signers, got %d", len(signers))
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close signer resources: %v", err)
	}

	wantOrder := []string{
		"private:" + explicitKeyPath,
		"agent:4",
		"private:" + suiteKeyPath,
		"private:" + homeDir + "/.ssh/id_rsa",
		"private:" + homeDir + "/.ssh/id_dsa",
		"private:" + homeDir + "/.ssh/id_ecdsa",
		"private:" + homeDir + "/.ssh/id_ed25519",
	}
	if !reflect.DeepEqual(callOrder, wantOrder) {
		t.Fatalf("Unexpected auth method call order.\nexpected: %v\ngot:      %v", wantOrder, callOrder)
	}
}
