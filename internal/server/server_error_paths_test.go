package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

func TestNewReturnsMalformedHostKeyError(t *testing.T) {
	t.Setenv("DTAIL_INTEGRATION_TEST_RUN_MODE", "")
	hostKeyFile := filepath.Join(t.TempDir(), "ssh_host_key")
	if err := os.WriteFile(hostKeyFile, []byte("not a private key"), 0o600); err != nil {
		t.Fatalf("write malformed host key: %v", err)
	}

	serverConfig := config.NewDefaultServerConfigForTest()
	serverConfig.HostKeyPath = hostKeyFile
	_, err := New(config.RuntimeConfig{
		Server: serverConfig,
		Common: &config.CommonConfig{SSHPort: 2222, CacheDir: t.TempDir()},
	}, serverTestLoggers, nil)
	if err == nil || !strings.Contains(err.Error(), "parse SSH host key") {
		t.Fatalf("New error = %v, want malformed host-key error", err)
	}
}

func TestStartReturnsListenError(t *testing.T) {
	server := &Server{cfg: config.RuntimeConfig{
		Server: &config.ServerConfig{SSHBindAddress: "127.0.0.1"},
		Common: &config.CommonConfig{SSHPort: 65536},
	}, logger: serverTestLoggers.Diagnostics}

	status, err := server.Start(context.Background())
	if err == nil || status != 1 {
		t.Fatalf("Start status=%d error=%v, want status 1 listen error", status, err)
	}
}

func TestStartCancellationStopsBlockedListen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	server := &Server{
		cfg: config.RuntimeConfig{
			Server: &config.ServerConfig{SSHBindAddress: "127.0.0.1"},
			Common: &config.CommonConfig{SSHPort: 2222},
		},
		logger: serverTestLoggers.Diagnostics,
		listen: func(ctx context.Context, network, address string) (net.Listener, error) {
			if network != "tcp" || address != "127.0.0.1:2222" {
				t.Errorf("listen target = %s %s", network, address)
			}
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	result := make(chan error, 1)
	go func() {
		_, err := server.Start(ctx)
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not stop a blocked listen after cancellation")
	}
}

func TestStartRejectsNilContext(t *testing.T) {
	var nilContext context.Context
	status, err := (&Server{}).Start(nilContext)
	if status != 1 || err == nil {
		t.Fatalf("Start(nil) = (%d, %v), want status 1 and error", status, err)
	}
}
