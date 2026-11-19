package clients

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
	sshclient "github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

// newDiscoveryTestClient returns a cat client for servers that is ready for
// makeConnections. With stub set, its connectionFactory records the servers
// it is asked to connect to and returns stub connectors instead of
// connecting; otherwise makeConnections builds real (not yet started) server
// connections, which parse each server address.
func newDiscoveryTestClient(servers string, serverless bool, stub *[]string) *baseClient {
	args := config.Args{Mode: omode.CatClient, What: "/etc/hostname", ServersStr: servers,
		Serverless: serverless}
	runtimeCfg := clientTestRuntimeConfig()
	c := &baseClient{
		mu:          newBaseClientMu(),
		Args:        args,
		cfg:         runtimeCfg,
		runtime:     newClientRuntimeBoundary(runtimeCfg, LoggerDependencies{}.normalized()),
		sessionSpec: NewSessionSpec(args),
		profile:     clientHandlerProfile(clientlog.NopLogger{}, false),
	}
	if stub != nil {
		c.connectionFactory = func(server string, _ []gossh.AuthMethod, _ sshclient.HostKeyCallback,
			spec SessionSpec, _ bool) connectors.Connector {
			*stub = append(*stub, server)
			return newReloadRetryConnector(server, spec)
		}
	}
	return c
}

// writeServerFile writes content to a server list file in a temporary
// directory and returns its path.
func writeServerFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "servers.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write server list file: %v", err)
	}
	return path
}

// TestMakeConnectionsIgnoresBlankServerEntries is the client-level
// regression test of a blank line in a server list file and of spaces around
// the commas of a server list: each used to abort the whole client with an
// empty or whitespace-wrapped server address.
func TestMakeConnectionsIgnoresBlankServerEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		servers func(*testing.T) string
		want    []string
	}{
		{name: "file with a blank line", want: []string{"alpha:2222", "beta"},
			servers: func(t *testing.T) string { return writeServerFile(t, "alpha:2222\n\nbeta\n") }},
		{name: "file path with surrounding whitespace", want: []string{"alpha", "beta"},
			servers: func(t *testing.T) string { return writeServerFile(t, "alpha\nbeta\n") + " " }},
		{name: "comma list with spaces", want: []string{"alpha", "beta"},
			servers: func(*testing.T) string { return "alpha, beta" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var dialled []string
			c := newDiscoveryTestClient(tt.servers(t), false, &dialled)
			if err := c.makeConnections(); err != nil {
				t.Fatalf("makeConnections() error = %v", err)
			}
			slices.Sort(dialled)
			if !slices.Equal(dialled, tt.want) || len(c.connections) != len(tt.want) {
				t.Fatalf("connections to %q (%d), want %q", dialled, len(c.connections), tt.want)
			}
		})
	}
}

// TestMakeConnectionsRejectsNoServer checks that a client fails when its
// discovery yields no server, instead of running no session and exiting 0
// with no output (and dtailhealth reporting OK). An empty server value with a
// discovery module fails the same way, not with the empty server address "".
// The check does not depend on serverless mode: a serverless client whose
// discovery yields nothing fails too. A serverless client with the empty
// server value still gets its single serverless session.
func TestMakeConnectionsRejectsNoServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		discovery  string
		serverless bool
		servers    func(*testing.T) string
	}{
		{name: "blank server value", servers: func(*testing.T) string { return " " }},
		{name: "only commas and spaces", servers: func(*testing.T) string { return " , ," }},
		{name: "file of only comment and blank lines",
			servers: func(t *testing.T) string { return writeServerFile(t, "# none\n\n  \n") }},
		{name: "regex without a discovery module", servers: func(*testing.T) string { return "/alpha/" }},
		{name: "empty server value with the comma module", discovery: "comma",
			servers: func(*testing.T) string { return "" }},
		{name: "blank server value with the comma module", discovery: "comma",
			servers: func(*testing.T) string { return " " }},
		{name: "serverless with a blank server value", serverless: true,
			servers: func(*testing.T) string { return " " }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var dialled []string
			c := newDiscoveryTestClient(tt.servers(t), tt.serverless, &dialled)
			c.Discovery = tt.discovery
			err := c.makeConnections()
			if !errors.Is(err, ErrNoServers) || !strings.Contains(err.Error(), "discover servers: no server to connect to") {
				t.Fatalf("makeConnections() error = %v, want %v", err, ErrNoServers)
			}
			if len(dialled) != 0 || len(c.connections) != 0 {
				t.Fatalf("connections to %q (%d), want none", dialled, len(c.connections))
			}
		})
	}

	t.Run("serverless", func(t *testing.T) {
		t.Parallel()
		var dialled []string
		c := newDiscoveryTestClient("", true, &dialled)
		if err := c.makeConnections(); err != nil {
			t.Fatalf("makeConnections() error = %v", err)
		}
		if !slices.Equal(dialled, []string{""}) || len(c.connections) != 1 {
			t.Fatalf("connections to %q (%d), want the single serverless one", dialled, len(c.connections))
		}
	})
}

// TestMakeConnectionsRejectsBuiltInModuleOptions checks that a COMMA or FILE
// discovery module with options fails the client before it dials anything,
// as a configuration error that is no ErrNoServers: "comma:server1,server2"
// with an empty server value used to query the empty serverless address (or,
// for a scheduled job, this dserver) instead of failing.
func TestMakeConnectionsRejectsBuiltInModuleOptions(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"comma:server1,server2", "FILE:/path/to/serverlist"} {
		var dialled []string
		c := newDiscoveryTestClient("", false, &dialled)
		c.Discovery = method
		err := c.makeConnections()
		if err == nil || errors.Is(err, ErrNoServers) ||
			!strings.Contains(err.Error(), "configure server discovery: ") ||
			!strings.Contains(err.Error(), "takes no options") {
			t.Fatalf("makeConnections(%q) error = %v, want a rejected discovery configuration", method, err)
		}
		if len(dialled) != 0 || len(c.connections) != 0 {
			t.Fatalf("makeConnections(%q) connected to %q (%d), want none", method, dialled, len(c.connections))
		}
	}
}

// TestMakeConnectionsStillRejectsBadServerEntries pins that discovery only
// trims surrounding whitespace: entries that are bad in themselves still
// reach the server address parser, which rejects them. A '#' after an entry
// of a server list file is no comment.
func TestMakeConnectionsStillRejectsBadServerEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		servers func(*testing.T) string
		want    string
	}{
		{name: "invalid port", servers: func(*testing.T) string { return " host:notaport" },
			want: `parse port in server address "host:notaport"`},
		{name: "inner whitespace", servers: func(*testing.T) string { return "alpha beta" },
			want: `invalid hostname "alpha beta"`},
		{name: "inline comment in a file",
			servers: func(t *testing.T) string { return writeServerFile(t, "host # c\n") },
			want:    `invalid hostname "host # c"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newDiscoveryTestClient(tt.servers(t), false, nil)
			err := c.makeConnections()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("makeConnections() error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestNewHealthClientRejectsNoServer checks that dtailhealth cannot be
// created for a server value naming no server, so that it reports CRITICAL
// (cmd/dtailhealth exits 2 on a construction error) instead of "OK: All
// fine" for a check that ran nowhere.
func TestNewHealthClientRejectsNoServer(t *testing.T) {
	t.Parallel()

	for _, servers := range []string{" ", writeServerFile(t, "# none\n")} {
		args := config.Args{ServersStr: servers, ConnectionsPerCPU: 1}
		args.TrustAllHosts = true
		args.KnownHostsPath = filepath.Join(t.TempDir(), "known_hosts")
		args.NoAuthKey = true
		client, err := NewHealthClient(args, clientTestRuntimeConfig(), LoggerDependencies{})
		if !errors.Is(err, ErrNoServers) || client != nil {
			t.Fatalf("NewHealthClient(%q) = %v, %v, want error %v", servers, client, err, ErrNoServers)
		}
	}
}
