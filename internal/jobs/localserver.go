package jobs

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
)

// lookupTimeout bounds the name lookup of a job's server.
const lookupTimeout = 2 * time.Second

// thisDServer recognises the server addresses that reach the dserver running
// the scheduler: the dserver listening on the SSHBindAddress and SSHPort of the
// scheduler's configuration, whose MaxConnections the scheduler knows.
type thisDServer struct {
	bind netip.Addr
	// bindAll is set when dserver listens on every address of the host. When
	// the bind address is not an IP address, bind is the zero Addr and no
	// address is recognised.
	bindAll bool
	port    int
	// lookup resolves a host name to its IP addresses.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// hostAddrs returns the IP addresses of the host's network interfaces.
	hostAddrs func() ([]netip.Addr, error)
}

func newThisDServer(cfg config.RuntimeConfig) thisDServer {
	d := thisDServer{
		port: config.DefaultSSHPort,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		hostAddrs: interfaceAddrs,
	}
	// A job's client dials a server without a port at the same port as
	// dserver listens at (see clients.newClientRuntimeBoundary).
	if cfg.Common != nil && cfg.Common.SSHPort > 0 {
		d.port = cfg.Common.SSHPort
	}
	if cfg.Server == nil {
		return d
	}
	if cfg.Server.SSHBindAddress == "" {
		d.bindAll = true
		return d
	}
	if bind, err := netip.ParseAddr(cfg.Server.SSHBindAddress); err == nil && bind.Zone() == "" {
		d.bind = bind.Unmap()
		d.bindAll = d.bind.IsUnspecified()
	}
	return d
}

func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	for _, addr := range addrs {
		if prefix, err := netip.ParsePrefix(addr.String()); err == nil {
			ips = append(ips, prefix.Addr().Unmap())
		}
	}
	return ips, nil
}

// reaches reports whether a connection to server, a job's server address,
// surely reaches this dserver: its port is dserver's port, and each IP address
// of its host is dserver's bind address or, when dserver listens on every
// address, a loopback, unspecified or network interface address of this host.
// A host name counts only when it resolves, and only to such addresses. When
// unsure, as for a malformed address or a failed lookup, it reports false.
func (d thisDServer) reaches(ctx context.Context, server string) bool {
	host, port, err := connectors.ParseServerAddress(server, d.port)
	if err != nil || port != d.port {
		return false
	}
	var ips []netip.Addr
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
		ips = []netip.Addr{ip}
	} else {
		lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
		ips, err = d.lookup(lookupCtx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			return false
		}
	}
	var hostAddrs []netip.Addr
	for _, ip := range ips {
		ip = ip.Unmap()
		if !d.bindAll {
			if ip != d.bind {
				return false
			}
			continue
		}
		if ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		if hostAddrs == nil {
			if hostAddrs, err = d.hostAddrs(); err != nil {
				return false
			}
		}
		if !slices.Contains(hostAddrs, ip) {
			return false
		}
	}
	return true
}
