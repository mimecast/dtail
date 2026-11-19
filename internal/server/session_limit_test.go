package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"

	gossh "golang.org/x/crypto/ssh"
)

// sessionLimitClient connects an SSH client to a Server's handleConnection
// over a loopback connection, without client authentication.
func sessionLimitClient(t *testing.T) gossh.Conn {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	sshConfig := &gossh.ServerConfig{NoClientAuth: true}
	sshConfig.AddHostKey(signer)
	s := &Server{
		cfg: config.RuntimeConfig{
			Server: &config.ServerConfig{
				MaxLineLength: 1024 * 1024,
				Permissions:   config.Permissions{Default: []string{"readfiles:^/nonexistent/.*$"}},
			},
			Common: &config.CommonConfig{},
		},
		logger:          logging.NopLogger{},
		stats:           newStats(2, nil),
		sshServerConfig: sshConfig,
		catLimiter:      make(chan struct{}, 2),
		tailLimiter:     make(chan struct{}, 2),
		authKeyStore:    authkey.New(time.Hour, 5),
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		s.stats.reservePreAuth()
		s.handleConnection(ctx, conn)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clientConn, chans, reqs, err := gossh.NewClientConn(conn, "test", &gossh.ClientConfig{
		User:            "alice",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("SSH client handshake: %v", err)
	}
	go gossh.DiscardRequests(reqs)
	go func() {
		for newChannel := range chans {
			_ = newChannel.Reject(gossh.Prohibited, "no channels to the client")
		}
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		cancel()
		_ = listener.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("handleConnection did not return")
		}
	})
	return clientConn
}

// A connection gets one session channel: a second one, which would be one
// more session beyond MaxConnections, is refused.
func TestHandleConnectionRefusesSecondSessionChannel(t *testing.T) {
	clientConn := sessionLimitClient(t)

	first, firstRequests, err := clientConn.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("first session channel: %v", err)
	}
	go gossh.DiscardRequests(firstRequests)
	defer func() { _ = first.Close() }()

	_, _, err = clientConn.OpenChannel("session", nil)
	var openErr *gossh.OpenChannelError
	if !errors.As(err, &openErr) || openErr.Reason != gossh.ResourceShortage {
		t.Fatalf("second session channel: error %v, want a resource shortage rejection", err)
	}
}

// A channel gets one shell: a second shell request is refused and ends the
// connection.
func TestHandleRequestsRefusesSecondShellRequest(t *testing.T) {
	clientConn := sessionLimitClient(t)

	channel, requests, err := clientConn.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("session channel: %v", err)
	}
	go gossh.DiscardRequests(requests)

	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil || !ok {
		t.Fatalf("first shell request: ok=%v err=%v, want it accepted", ok, err)
	}
	if ok, err = channel.SendRequest("shell", true, nil); ok {
		t.Fatalf("second shell request was accepted (err=%v)", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- clientConn.Wait() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open after a second shell request")
	}
}
