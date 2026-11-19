package connectors

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/logging"
	sessionspec "github.com/mimecast/dtail/internal/session"

	"golang.org/x/crypto/ssh"
)

const testHandshakeTimeout = 300 * time.Millisecond

// funcHostKeyCallback is a client.HostKeyCallback running check, e.g. to
// stand in for a user taking long to answer the unknown host question.
type funcHostKeyCallback struct {
	check ssh.HostKeyCallback
}

func (f funcHostKeyCallback) Wrap(context.Context) ssh.HostKeyCallback { return f.check }
func (funcHostKeyCallback) Untrusted(string) bool                      { return false }
func (funcHostKeyCallback) PromptAddHosts(context.Context)             {}

// listen returns a listener whose accepted connections serve is run on.
func listen(t *testing.T, serve func(net.Conn)) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
			go serve(conn)
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), port
}

// serveSSH runs an SSH server on conn that answers global requests.
func serveSSH(t *testing.T, config *ssh.ServerConfig) func(net.Conn) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config.AddHostKey(signer)
	return func(conn net.Conn) {
		_, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for newChannel := range chans {
			_ = newChannel.Reject(ssh.Prohibited, "test")
		}
	}
}

func newHandshakeTestConnection(t *testing.T, address string, port int,
	hostKeyCallback funcHostKeyCallback, auth []ssh.AuthMethod) *ServerConnection {
	t.Helper()
	c, err := NewServerConnection(address, "user", auth, hostKeyCallback,
		&mockHandler{}, nil, sessionspec.Spec{}, false, "", true,
		testSSHSettings{port: port, timeout: time.Second, handshakeTimeout: testHandshakeTimeout},
		logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func acceptHostKey(string, net.Addr, ssh.PublicKey) error { return nil }

// handshakeWithin runs the handshake of c and fails the test if it takes
// longer than limit.
func handshakeWithin(t *testing.T, c *ServerConnection, address string, limit time.Duration) (*ssh.Client, error) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	type result struct {
		client *ssh.Client
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		client, err := c.handshake(context.Background(), conn, address)
		resultCh <- result{client, err}
	}()
	select {
	case r := <-resultCh:
		return r.client, r.err
	case <-time.After(limit):
		t.Fatalf("SSH handshake still running after %v", limit)
		return nil, nil
	}
}

// A host that accepts the TCP connection but never says anything failed
// to time out: dial hung forever, the connect timeout only bounds the TCP
// connect.
func TestDialTimesOutOnSilentServer(t *testing.T) {
	address, port := listen(t, func(net.Conn) {})
	c := newHandshakeTestConnection(t, address, port, funcHostKeyCallback{acceptHostKey}, nil)

	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	go func() { errCh <- c.dial(ctx, cancel, make(chan struct{}, 1), make(chan struct{}, 1)) }()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "no handshake within "+testHandshakeTimeout.String()) {
			t.Fatalf("expected a handshake timeout error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed < testHandshakeTimeout {
			t.Fatalf("dial failed after %v, before the handshake timeout", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dial still hangs 5 s after the TCP connect to a server that never answers")
	}
}

// The user may take longer than the handshake timeout to answer the
// unknown host question: the host key check must not be cut off, and the
// deadline must be gone once the handshake is done.
func TestHandshakeTimeoutExcludesHostKeyCheck(t *testing.T) {
	address, port := listen(t, serveSSH(t, &ssh.ServerConfig{NoClientAuth: true}))
	slowAnswer := func(string, net.Addr, ssh.PublicKey) error {
		time.Sleep(3 * testHandshakeTimeout)
		return nil
	}
	c := newHandshakeTestConnection(t, address, port, funcHostKeyCallback{slowAnswer}, nil)

	client, err := handshakeWithin(t, c, address, 5*time.Second)
	if err != nil {
		t.Fatalf("handshake with a slow host key answer failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	time.Sleep(2 * testHandshakeTimeout)
	if _, _, err := client.SendRequest("test@dtail", true, nil); err != nil {
		t.Fatalf("connection failed after the handshake, deadline not cleared: %v", err)
	}
}

// The deadline resumes after the host key check: a server that stalls
// during authentication still times out.
func TestHandshakeTimesOutAfterHostKeyCheck(t *testing.T) {
	stalled := make(chan struct{})
	t.Cleanup(func() { close(stalled) })
	address, port := listen(t, serveSSH(t, &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			<-stalled
			return nil, errors.New("denied")
		},
	}))
	c := newHandshakeTestConnection(t, address, port, funcHostKeyCallback{acceptHostKey},
		[]ssh.AuthMethod{ssh.Password("secret")})

	_, err := handshakeWithin(t, c, address, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "no handshake within") {
		t.Fatalf("expected a handshake timeout error, got %v", err)
	}
}

// A rejected host key is a handshake failure, not a timeout.
func TestHandshakeRejectedHostKeyIsNoTimeout(t *testing.T) {
	address, port := listen(t, serveSSH(t, &ssh.ServerConfig{NoClientAuth: true}))
	reject := func(string, net.Addr, ssh.PublicKey) error { return errors.New("untrusted host") }
	c := newHandshakeTestConnection(t, address, port, funcHostKeyCallback{reject}, nil)

	_, err := handshakeWithin(t, c, address, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "untrusted host") ||
		strings.Contains(err.Error(), "no handshake within") {
		t.Fatalf("expected the host key rejection, got %v", err)
	}
}

// deadlineRecorder records the deadlines set on a connection.
type deadlineRecorder struct {
	net.Conn
	deadlines []time.Time
}

func (r *deadlineRecorder) SetDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

// A re-key after the handshake runs the host key callback again; it must
// not arm a deadline on the established connection.
func TestHandshakeDeadlineInactiveAfterFinish(t *testing.T) {
	conn := &deadlineRecorder{}
	deadline, err := startHandshakeDeadline(conn, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if expired, err := deadline.finish(); expired || err != nil {
		t.Fatalf("finish = %v, %v; want not expired", expired, err)
	}
	set := len(conn.deadlines)
	if !conn.deadlines[set-1].IsZero() {
		t.Fatalf("finish left deadline %v", conn.deadlines[set-1])
	}
	check := deadline.wrapHostKeyCallback(acceptHostKey)
	if err := check("host", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(conn.deadlines) != set {
		t.Fatalf("host key callback after finish set deadlines %v", conn.deadlines[set:])
	}
}

// pause and resume keep the time that was left, not a fresh timeout.
func TestHandshakeDeadlineResumesRemainingTime(t *testing.T) {
	conn := &deadlineRecorder{}
	deadline, err := startHandshakeDeadline(conn, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	deadline.pause()
	if !conn.deadlines[len(conn.deadlines)-1].IsZero() {
		t.Fatal("pause did not clear the deadline")
	}
	pausedAt := time.Now()
	deadline.resume()
	resumed := conn.deadlines[len(conn.deadlines)-1]
	if left := resumed.Sub(pausedAt); left > time.Minute-40*time.Millisecond || left <= 0 {
		t.Fatalf("resumed with %v left, want the remaining time of the minute", left)
	}
}
