package ssh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/mimecast/dtail/internal/logging"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// closerFunc adapts a plain func to io.Closer so callers can always
// unconditionally `defer closer.Close()` regardless of the code path taken.
type closerFunc func() error

// Close implements io.Closer.
func (f closerFunc) Close() error {
	if f == nil {
		return nil
	}
	return f()
}

// noopCloser is returned when there is no resource to release. Using a single
// shared value keeps allocations out of the fast error paths.
var noopCloser io.Closer = closerFunc(func() error { return nil })

// dialAgent dials the local ssh-agent unix socket. It is a package-level
// variable so unit tests can replace it with a fake for deterministic
// error-path coverage (see ssh_agent_test.go).
var dialAgent = func(addr string) (net.Conn, error) {
	// Use context-aware dialing for SSH agent connection (local Unix socket).
	// 2-second timeout is reasonable for local socket connections.
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return dialer.DialContext(ctx, "unix", addr)
}

// GeneratePrivateRSAKey is used by the server to generate its key.
func GeneratePrivateRSAKey(size int) (*rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, size)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}
	if err = privateKey.Validate(); err != nil {
		return nil, fmt.Errorf("failed to validate generated RSA key: %w", err)
	}
	return privateKey, nil
}

// EncodePrivateKeyToPEM is a helper function for converting a key to PEM format.
func EncodePrivateKeyToPEM(privateKey *rsa.PrivateKey) []byte {
	derFormat := x509.MarshalPKCS1PrivateKey(privateKey)

	block := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   derFormat,
	}
	return pem.EncodeToMemory(&block)
}

// Agent used for SSH auth. The returned io.Closer owns the underlying
// ssh-agent connection and MUST be closed by the caller once the returned
// AuthMethod is no longer needed (i.e. after the final SSH handshake that
// uses it has completed).
func Agent(logger logging.Logger) (gossh.AuthMethod, io.Closer, error) {
	return AgentWithKeyIndex(-1, logger)
}

// AgentSignersWithKeyIndex returns SSH agent signers together with an
// io.Closer that owns the underlying ssh-agent connection.
//
// The agent signers call back to the agent over the returned connection on
// every Sign() invocation, so the connection must stay open for as long as
// the signers are in use (typically until the SSH handshakes that consume
// them are complete). The caller is responsible for invoking Close() on the
// returned io.Closer to release file descriptors and the agent goroutine.
//
// The returned io.Closer is always non-nil, including on error paths, so
// callers can unconditionally `defer closer.Close()`.
//
// If keyIndex is -1, all keys are used. Otherwise, only the specified key is
// used.
func AgentSignersWithKeyIndex(keyIndex int, logger logging.Logger) ([]gossh.Signer, io.Closer, error) {
	logger = logging.OrNop(logger)
	sshAgent, err := dialAgent(os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, noopCloser, fmt.Errorf("failed to connect to SSH agent: %w", err)
	}

	// Ensure the connection is released on every error path. The success
	// path hands ownership to the caller by setting owned to nil right
	// before the return.
	owned := sshAgent
	defer func() {
		if owned != nil {
			_ = owned.Close()
		}
	}()

	agentClient := agent.NewClient(sshAgent)
	keys, err := agentClient.List()
	if err != nil {
		return nil, noopCloser, fmt.Errorf("failed to list SSH agent keys: %w", err)
	}
	for i, key := range keys {
		logger.Debug("Public key", i, key)
	}

	signers, err := agentClient.Signers()
	if err != nil {
		return nil, noopCloser, fmt.Errorf("failed to load SSH agent signers: %w", err)
	}

	// If no specific key index requested, use all keys (backwards compatible default)
	if keyIndex < 0 {
		owned = nil
		return signers, sshAgent, nil
	}

	// Use only the specified key index (0-based)
	if keyIndex >= len(signers) {
		return nil, noopCloser, fmt.Errorf("key index %d out of range (agent has %d signers)", keyIndex, len(signers))
	}

	logger.Debug("Using SSH agent key at index", keyIndex)
	owned = nil
	return []gossh.Signer{signers[keyIndex]}, sshAgent, nil
}

// AgentWithKeyIndex used for SSH auth with a specific key index from the agent.
// The caller owns the returned io.Closer; see AgentSignersWithKeyIndex for
// lifetime semantics.
// If keyIndex is -1, all keys are used. Otherwise, only the specified key is used.
func AgentWithKeyIndex(keyIndex int, logger logging.Logger) (gossh.AuthMethod, io.Closer, error) {
	signers, closer, err := AgentSignersWithKeyIndex(keyIndex, logger)
	if err != nil {
		return nil, closer, err
	}
	return gossh.PublicKeys(signers...), closer, nil
}

// PrivateKeySigner returns an SSH signer from the provided private key file.
func PrivateKeySigner(keyFile string) (gossh.Signer, error) {
	buffer, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	key, err := gossh.ParsePrivateKey(buffer)
	if err != nil {
		var passphraseMissingErr *gossh.PassphraseMissingError
		if !errors.As(err, &passphraseMissingErr) {
			return nil, err
		}

		passphrase := os.Getenv("DTAIL_KEY_PASSPHRASE")
		if passphrase == "" {
			return nil, err
		}

		key, err = gossh.ParsePrivateKeyWithPassphrase(buffer, []byte(passphrase))
		if err != nil {
			return nil, err
		}
	}
	return key, nil
}

// KeyFile returns the key as a SSH auth method.
func KeyFile(keyFile string) (gossh.AuthMethod, error) {
	key, err := PrivateKeySigner(keyFile)
	if err != nil {
		return nil, err
	}

	// Key phrase support disabled as password will be printed to stdout!
	/*
		if err == nil {
			return gossh.PublicKeys(key), nil
		}

		keyPhrase := EnterKeyPhrase(keyFile)
		key, err = gossh.ParsePrivateKeyWithPassphrase(buffer, keyPhrase)
		if err != nil {
			return nil, err
		}
	*/

	return gossh.PublicKeys(key), nil
}

// PrivateKey returns the private key as a SSH auth method.
func PrivateKey(keyFile string) (gossh.AuthMethod, error) {
	signer, err := KeyFile(keyFile)
	if err != nil {
		return nil, err
	}
	return gossh.AuthMethod(signer), nil
}
