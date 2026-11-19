package jobs

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestThisDServerReachesOnlyItself(t *testing.T) {
	names := map[string][]netip.Addr{
		"localhost":      {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")},
		"myhost":         {netip.MustParseAddr("192.168.1.5")},
		"myhost-v6":      {netip.MustParseAddr("2001:db8::5")},
		"mixed":          {netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("192.168.1.6")},
		"otherhost":      {netip.MustParseAddr("192.168.1.6")},
		"bound":          {netip.MustParseAddr("10.0.0.1")},
		"localhost-v4":   {netip.MustParseAddr("127.0.0.1")},
		"resolves-empty": {},
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if addrs, ok := names[host]; ok {
			return addrs, nil
		}
		return nil, errors.New("no such host")
	}
	hostAddrs := func() ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"),
			netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("2001:db8::5")}, nil
	}

	tests := []struct {
		name   string
		bind   string
		port   int
		server string
		want   bool
	}{
		// dserver listening on every address of the host.
		{name: "loopback with the port", bind: "0.0.0.0", server: "127.0.0.1:2222", want: true},
		{name: "loopback without a port", bind: "0.0.0.0", server: "127.0.0.1", want: true},
		{name: "other loopback", bind: "0.0.0.0", server: "127.0.0.2:2222", want: true},
		{name: "loopback, other port", bind: "0.0.0.0", server: "127.0.0.1:2223"},
		{name: "loopback, dserver on another port", bind: "0.0.0.0", port: 3000, server: "127.0.0.1:2222"},
		{name: "loopback, port of dserver", bind: "0.0.0.0", port: 3000, server: "127.0.0.1:3000", want: true},
		// A job dials a server without a port at dserver's port.
		{name: "loopback without a port, dserver on another port", bind: "0.0.0.0", port: 3000,
			server: "127.0.0.1", want: true},
		{name: "IPv6 loopback", bind: "0.0.0.0", server: "[::1]:2222", want: true},
		{name: "IPv6 loopback without a port", bind: "::", server: "::1", want: true},
		{name: "unspecified address", bind: "0.0.0.0", server: "0.0.0.0", want: true},
		{name: "empty bind address", bind: "", server: "127.0.0.1", want: true},
		{name: "localhost", bind: "0.0.0.0", server: "localhost:2222", want: true},
		{name: "interface address", bind: "0.0.0.0", server: "192.168.1.5", want: true},
		{name: "IPv6 interface address", bind: "0.0.0.0", server: "[2001:db8::5]:2222", want: true},
		{name: "host name of an interface address", bind: "0.0.0.0", server: "myhost", want: true},
		{name: "host name of an IPv6 interface address", bind: "0.0.0.0", server: "myhost-v6", want: true},
		{name: "other host", bind: "0.0.0.0", server: "192.168.1.6"},
		{name: "host name of another host", bind: "0.0.0.0", server: "otherhost"},
		{name: "host name also of another host", bind: "0.0.0.0", server: "mixed"},
		{name: "unknown host name", bind: "0.0.0.0", server: "nosuchhost"},
		{name: "host name without addresses", bind: "0.0.0.0", server: "resolves-empty"},
		{name: "IPv6 link local with zone", bind: "0.0.0.0", server: "[fe80::1%eth0]:2222"},
		{name: "malformed address", bind: "0.0.0.0", server: "127.0.0.1:x"},
		{name: "empty address", bind: "0.0.0.0", server: ""},
		// dserver listening on one address.
		{name: "bind address", bind: "10.0.0.1", server: "10.0.0.1:2222", want: true},
		{name: "host name of the bind address", bind: "10.0.0.1", server: "bound", want: true},
		{name: "loopback, bound elsewhere", bind: "10.0.0.1", server: "127.0.0.1"},
		{name: "interface address, bound elsewhere", bind: "10.0.0.1", server: "192.168.1.5"},
		{name: "bound to loopback", bind: "127.0.0.1", server: "127.0.0.1", want: true},
		{name: "IPv4-mapped bind address", bind: "127.0.0.1", server: "[::ffff:127.0.0.1]:2222", want: true},
		{name: "bound to an IPv4-mapped address", bind: "::ffff:127.0.0.1", server: "127.0.0.1", want: true},
		{name: "other loopback, bound to loopback", bind: "127.0.0.1", server: "127.0.0.2"},
		// localhost may resolve to ::1, which dserver on 127.0.0.1 does not
		// listen on.
		{name: "localhost, bound to IPv4 loopback", bind: "127.0.0.1", server: "localhost"},
		{name: "IPv4 localhost, bound to IPv4 loopback", bind: "127.0.0.1", server: "localhost-v4", want: true},
		{name: "unspecified address, bound to loopback", bind: "127.0.0.1", server: "0.0.0.0"},
		{name: "bind address not an IP address", bind: "localhost", server: "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.RuntimeConfig{Server: &config.ServerConfig{SSHBindAddress: tt.bind}}
			if tt.port != 0 {
				cfg.Common = &config.CommonConfig{SSHPort: tt.port}
			}
			d := newThisDServer(cfg)
			d.lookup = lookup
			d.hostAddrs = hostAddrs
			if got := d.reaches(context.Background(), tt.server); got != tt.want {
				t.Errorf("reaches(%q) with bind address %q = %v, want %v", tt.server, tt.bind, got, tt.want)
			}
		})
	}
}

// Without the addresses of the network interfaces, only loopback and
// unspecified addresses reach dserver.
func TestThisDServerWithoutInterfaceAddresses(t *testing.T) {
	d := newThisDServer(config.RuntimeConfig{Server: &config.ServerConfig{SSHBindAddress: "0.0.0.0"}})
	d.hostAddrs = func() ([]netip.Addr, error) { return nil, errors.New("no interfaces") }
	if !d.reaches(context.Background(), "127.0.0.1") {
		t.Error("loopback does not reach dserver")
	}
	if d.reaches(context.Background(), "192.168.1.5") {
		t.Error("an address not known to be the host's reaches dserver")
	}
}
