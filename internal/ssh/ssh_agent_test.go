package ssh

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/mimecast/dtail/internal/logging"

	"golang.org/x/crypto/ssh/agent"
)

// countingConn wraps a net.Conn and tracks how many times Close was called.
type countingConn struct {
	net.Conn
	closeCount atomic.Int32
}

func (c *countingConn) Close() error {
	c.closeCount.Add(1)
	return c.Conn.Close()
}

// pipePair returns a client/server net.Conn pair and wraps the client side
// in a counting closer so tests can assert Close was invoked.
func pipePair() (*countingConn, net.Conn) {
	client, server := net.Pipe()
	return &countingConn{Conn: client}, server
}

// newFakeAgent starts a serving ssh-agent on the server side of the pipe.
// Callers must close the returned server conn when done (or rely on client Close
// propagating through net.Pipe).
func newFakeAgent(t *testing.T, server net.Conn, keyring agent.Agent) {
	t.Helper()
	go func() {
		_ = agent.ServeAgent(keyring, server)
		_ = server.Close()
	}()
}

func withDialAgent(t *testing.T, dial func() (net.Conn, error)) {
	t.Helper()
	orig := dialAgent
	dialAgent = func(_ string) (net.Conn, error) {
		return dial()
	}
	t.Cleanup(func() { dialAgent = orig })
}

// TestAgentSignersWithKeyIndexClosesConnOnDialListError exercises the error
// path where agent.List fails. The ssh-agent connection must not be leaked.
func TestAgentSignersWithKeyIndexClosesConnOnListError(t *testing.T) {
	client, server := pipePair()
	// Close the server side immediately so agentClient.List() returns an error.
	_ = server.Close()

	withDialAgent(t, func() (net.Conn, error) { return client, nil })

	signers, closer, err := AgentSignersWithKeyIndex(-1, logging.NopLogger{})
	if err == nil {
		t.Fatalf("expected error from agent.List when server closed, got nil")
	}
	if signers != nil {
		t.Fatalf("expected nil signers on error, got %d", len(signers))
	}
	if closer == nil {
		t.Fatalf("expected non-nil closer even on error path")
	}
	// closer may be a no-op on error (ownership already released internally),
	// but calling Close must be safe.
	_ = closer.Close()

	if got := client.closeCount.Load(); got < 1 {
		t.Fatalf("expected underlying agent conn to be closed on error path, closeCount=%d", got)
	}
}

// TestAgentSignersWithKeyIndexClosesConnOnDialError verifies that when the
// initial dial fails no conn is ever created and the returned closer is safe.
func TestAgentSignersWithKeyIndexClosesConnOnDialError(t *testing.T) {
	dialErr := errors.New("dial failed")
	withDialAgent(t, func() (net.Conn, error) { return nil, dialErr })

	signers, closer, err := AgentSignersWithKeyIndex(-1, logging.NopLogger{})
	if err == nil {
		t.Fatalf("expected dial error, got nil")
	}
	if signers != nil {
		t.Fatalf("expected nil signers on dial error, got %d", len(signers))
	}
	if closer == nil {
		t.Fatalf("expected non-nil closer even on dial error")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("closer.Close on dial error should be a no-op, got %v", err)
	}
}

// TestAgentSignersWithKeyIndexReturnsOwnerCloserOnSuccess verifies the happy
// path where the caller takes ownership of the agent connection via io.Closer.
func TestAgentSignersWithKeyIndexReturnsOwnerCloserOnSuccess(t *testing.T) {
	client, server := pipePair()
	keyring := agent.NewKeyring()
	newFakeAgent(t, server, keyring)

	withDialAgent(t, func() (net.Conn, error) { return client, nil })

	_, closer, err := AgentSignersWithKeyIndex(-1, logging.NopLogger{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closer == nil {
		t.Fatalf("expected non-nil closer on success")
	}
	if got := client.closeCount.Load(); got != 0 {
		t.Fatalf("underlying conn must stay open on success until caller closes, closeCount=%d", got)
	}

	if err := closer.Close(); err != nil {
		t.Fatalf("closer.Close returned error: %v", err)
	}
	if got := client.closeCount.Load(); got < 1 {
		t.Fatalf("expected closer to close the underlying agent conn, closeCount=%d", got)
	}
}

// TestAgentSignersWithKeyIndexOutOfRangeClosesConn verifies that when the
// requested key index exceeds the number of agent signers the connection is
// released.
func TestAgentSignersWithKeyIndexOutOfRangeClosesConn(t *testing.T) {
	client, server := pipePair()
	keyring := agent.NewKeyring() // empty keyring => no signers
	newFakeAgent(t, server, keyring)

	withDialAgent(t, func() (net.Conn, error) { return client, nil })

	_, closer, err := AgentSignersWithKeyIndex(0, logging.NopLogger{})
	if err == nil {
		t.Fatalf("expected out-of-range error on empty keyring, got nil")
	}
	if closer == nil {
		t.Fatalf("expected non-nil closer on out-of-range error")
	}
	_ = closer.Close()
	if got := client.closeCount.Load(); got < 1 {
		t.Fatalf("expected conn close on out-of-range error, closeCount=%d", got)
	}
}

// Compile-time sanity: the returned closer must satisfy io.Closer.
var _ io.Closer = (*countingConn)(nil)
