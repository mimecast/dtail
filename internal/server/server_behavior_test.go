package server

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"

	gossh "golang.org/x/crypto/ssh"
)

type testConnMetadata struct {
	user       string
	remoteAddr net.Addr
}

func (m testConnMetadata) User() string         { return m.user }
func (testConnMetadata) SessionID() []byte      { return nil }
func (testConnMetadata) ClientVersion() []byte  { return nil }
func (testConnMetadata) ServerVersion() []byte  { return nil }
func (m testConnMetadata) RemoteAddr() net.Addr { return m.remoteAddr }
func (testConnMetadata) LocalAddr() net.Addr    { return testAddr("127.0.0.1:2222") }

type testAddr string

func (testAddr) Network() string  { return "tcp" }
func (a testAddr) String() string { return string(a) }

type scriptedListener struct {
	mu      sync.Mutex
	steps   []func() (net.Conn, error)
	address net.Addr
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.steps) == 0 {
		return nil, io.EOF
	}
	step := l.steps[0]
	l.steps = l.steps[1:]
	return step()
}

func (*scriptedListener) Close() error { return nil }

func (l *scriptedListener) Addr() net.Addr {
	if l.address != nil {
		return l.address
	}
	return testAddr("127.0.0.1:0")
}

type deadlineErrorConn struct {
	closed bool
}

func (*deadlineErrorConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*deadlineErrorConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *deadlineErrorConn) Close() error                   { c.closed = true; return nil }
func (*deadlineErrorConn) LocalAddr() net.Addr              { return testAddr("127.0.0.1:2222") }
func (*deadlineErrorConn) RemoteAddr() net.Addr             { return testAddr("127.0.0.1:1234") }
func (*deadlineErrorConn) SetDeadline(time.Time) error      { return errors.New("deadline failed") }
func (*deadlineErrorConn) SetReadDeadline(time.Time) error  { return nil }
func (*deadlineErrorConn) SetWriteDeadline(time.Time) error { return nil }

func TestInternalUserAuthenticationPolicies(t *testing.T) {
	t.Parallel()

	scheduled := config.Scheduled{}
	scheduled.Name = "nightly-secret"
	scheduled.AllowFrom = []string{"127.0.0.1"}
	continuous := config.Continuous{}
	continuous.Name = "follow-secret"
	continuous.AllowFrom = []string{"127.0.0.1"}
	s := &Server{
		cfg: config.RuntimeConfig{Server: &config.ServerConfig{
			Schedule:    []config.Scheduled{scheduled},
			Continuous:  []config.Continuous{continuous},
			Permissions: config.Permissions{Default: []string{"^/.*"}},
		}},
		logger: logging.NopLogger{},
	}
	s.authStrategies = s.newAuthStrategies()

	tests := []struct {
		name       string
		user       string
		secret     string
		remoteAddr net.Addr
		wantOK     bool
	}{
		{name: "health accepted", user: config.HealthUser, secret: config.HealthUser, remoteAddr: testAddr("127.0.0.1:1234"), wantOK: true},
		{name: "health bad secret", user: config.HealthUser, secret: "wrong", remoteAddr: testAddr("127.0.0.1:1234")},
		{name: "scheduled accepted from allowlist", user: config.ScheduleUser, secret: scheduled.Name, remoteAddr: testAddr("127.0.0.1:1234"), wantOK: true},
		{name: "scheduled bad secret", user: config.ScheduleUser, secret: "wrong", remoteAddr: testAddr("127.0.0.1:1234")},
		{name: "scheduled address denied", user: config.ScheduleUser, secret: scheduled.Name, remoteAddr: testAddr("192.0.2.10:1234")},
		{name: "continuous accepted from allowlist", user: config.ContinuousUser, secret: continuous.Name, remoteAddr: testAddr("127.0.0.1:1234"), wantOK: true},
		{name: "unknown account has no password strategy", user: "alice", secret: "anything", remoteAddr: testAddr("127.0.0.1:1234")},
		{name: "unparseable address is denied", user: config.ScheduleUser, secret: scheduled.Name, remoteAddr: testAddr("invalid-address")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.Callback(context.Background(), testConnMetadata{user: tt.user, remoteAddr: tt.remoteAddr}, []byte(tt.secret))
			if tt.wantOK && err != nil {
				t.Fatalf("Callback() error = %v, want authorization", err)
			}
			if !tt.wantOK && err == nil {
				t.Fatal("Callback() authorized denied credentials")
			}
		})
	}
}

func TestBackgroundCanSSHSkipsUnresolvableAllowlistEntries(t *testing.T) {
	t.Parallel()

	s := &Server{logger: logging.NopLogger{}}
	if s.backgroundCanSSH(context.Background(), nil, "secret", "127.0.0.1", "secret", []string{"%%%", "127.0.0.1"}) != true {
		t.Fatal("backgroundCanSSH() did not continue past an invalid allowlist entry")
	}
	if s.backgroundCanSSH(context.Background(), nil, "wrong", "127.0.0.1", "secret", []string{"127.0.0.1"}) {
		t.Fatal("backgroundCanSSH() accepted the wrong secret")
	}
}

func TestBackgroundCanSSHCancellationStopsBlockedDNSLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	s := &Server{
		logger: logging.NopLogger{},
		lookupIPAddr: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			if host != "blocked.example" {
				t.Errorf("lookup host = %q, want blocked.example", host)
			}
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	result := make(chan bool, 1)
	go func() {
		result <- s.backgroundCanSSH(ctx, nil, "secret", "127.0.0.1", "secret", []string{"blocked.example"})
	}()
	<-entered
	cancel()
	select {
	case authorized := <-result:
		if authorized {
			t.Fatal("backgroundCanSSH authorized after canceled DNS lookup")
		}
	case <-time.After(time.Second):
		t.Fatal("backgroundCanSSH did not stop blocked DNS lookup after cancellation")
	}
}

func TestServerLoggerFallbacks(t *testing.T) {
	t.Parallel()

	s := &Server{}
	if _, ok := s.log().(logging.NopLogger); !ok {
		t.Fatalf("log() = %T, want NopLogger", s.log())
	}
	if _, ok := s.readerLog().(logging.NopLogger); !ok {
		t.Fatalf("readerLog() = %T, want fallback NopLogger", s.readerLog())
	}
	reader := &captureServerLogger{}
	s.readerLogger = reader
	if s.readerLog() != reader {
		t.Fatalf("readerLog() = %T, want configured reader logger", s.readerLog())
	}
}

type captureServerLogger struct{ logging.NopLogger }

func TestListenerLoopHandlesErrorsAndConnectionLimit(t *testing.T) {
	t.Parallel()

	t.Run("returns after canceled accept error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		listener := &scriptedListener{steps: []func() (net.Conn, error){
			func() (net.Conn, error) { return nil, io.EOF },
		}}
		(&Server{logger: logging.NopLogger{}}).listenerLoop(ctx, listener)
	})

	t.Run("continues after transient accept error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		secondAccept := false
		listener := &scriptedListener{steps: []func() (net.Conn, error){
			func() (net.Conn, error) { return nil, errors.New("temporary accept failure") },
			func() (net.Conn, error) {
				secondAccept = true
				cancel()
				return nil, io.EOF
			},
		}}
		(&Server{logger: logging.NopLogger{}}).listenerLoop(ctx, listener)
		if !secondAccept {
			t.Fatal("listenerLoop returned after a transient error without retrying Accept")
		}
	})

	t.Run("closes connection when limit is full", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		conn := &deadlineErrorConn{}
		server := &Server{logger: logging.NopLogger{}, stats: newStats(1, nil)}
		server.stats.currentConnections = 1
		listener := &scriptedListener{steps: []func() (net.Conn, error){
			func() (net.Conn, error) { return conn, nil },
			func() (net.Conn, error) { cancel(); return nil, io.EOF },
		}}
		server.listenerLoop(ctx, listener)
		if !conn.closed {
			t.Fatal("listenerLoop() did not close a connection rejected by the limit")
		}
	})
}

func TestHandleConnectionReleasesPreAuthOnEarlyFailures(t *testing.T) {
	t.Parallel()

	t.Run("deadline failure", func(t *testing.T) {
		conn := &deadlineErrorConn{}
		s := &Server{logger: logging.NopLogger{}, stats: newStats(2, nil)}
		s.stats.reservePreAuth()
		s.handleConnection(context.Background(), conn)
		if s.stats.preAuthConnections != 0 || !conn.closed {
			t.Fatalf("failure cleanup = pre-auth:%d closed:%v, want 0 and true", s.stats.preAuthConnections, conn.closed)
		}
	})

	t.Run("SSH handshake failure", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		if err := clientConn.Close(); err != nil {
			t.Fatalf("close client pipe: %v", err)
		}
		s := &Server{
			logger:          logging.NopLogger{},
			stats:           newStats(2, nil),
			sshServerConfig: &gossh.ServerConfig{},
		}
		s.stats.reservePreAuth()
		s.handleConnection(context.Background(), serverConn)
		if s.stats.preAuthConnections != 0 {
			t.Fatalf("pre-auth connections = %d, want 0", s.stats.preAuthConnections)
		}
	})
}

func TestHandleConnectionCancellationInterruptsSSHHandshake(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	s := &Server{
		logger:          logging.NopLogger{},
		stats:           newStats(2, nil),
		sshServerConfig: &gossh.ServerConfig{},
	}
	s.stats.reservePreAuth()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.handleConnection(ctx, serverConn)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled connection context did not interrupt SSH handshake")
	}
	if s.stats.preAuthConnections != 0 {
		t.Fatalf("pre-auth connections = %d after cancellation, want 0", s.stats.preAuthConnections)
	}
}

func TestHandleRequests(t *testing.T) {
	t.Parallel()

	s := &Server{logger: logging.NopLogger{}}
	t.Run("closed request stream", func(t *testing.T) {
		requests := make(chan *gossh.Request)
		close(requests)
		if err := s.handleRequests(context.Background(), nil, requests, nil, nil); err != nil {
			t.Fatalf("handleRequests() error = %v", err)
		}
	})

	t.Run("unknown request closes session", func(t *testing.T) {
		requests := make(chan *gossh.Request, 1)
		requests <- &gossh.Request{Type: "exec", WantReply: false, Payload: []byte("malformed")}
		close(requests)
		err := s.handleRequests(context.Background(), nil, requests, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "unknown request received|exec") {
			t.Fatalf("handleRequests() error = %v, want unknown-request context", err)
		}
	})
}
