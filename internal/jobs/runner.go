// Package jobs runs dserver's scheduled and continuous MapReduce client workloads.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

// Runner owns the background workloads configured for one dserver process.
type Runner struct {
	scheduler  *scheduler
	continuous *continuous
}

// New returns a background job runner.
func New(cfg config.RuntimeConfig, loggers clients.LoggerDependencies, colorizers ...*brush.Brush) *Runner {
	colorizer := firstColorizer(colorizers)
	return &Runner{
		scheduler:  newScheduler(cfg, loggers, colorizer),
		continuous: newContinuous(cfg, loggers, colorizer),
	}
}

// jobServersStr returns the client's --servers value for a scheduled or
// continuous job with the configured servers: the servers trimmed of
// surrounding whitespace, with empty entries dropped, joined with commas. If
// no server is left (no Servers, or only blank entries), it returns the
// address of this dserver (see thisDServerAddress), as an empty Servers means
// this dserver itself. Without the trimming a list like [" "] became the
// --servers value " ", which names no server and so failed every run.
func jobServersStr(servers []string, bindAddress string) string {
	kept := make([]string, 0, len(servers))
	for _, server := range servers {
		if server = strings.TrimSpace(server); server != "" {
			kept = append(kept, server)
		}
	}
	if len(kept) == 0 {
		return thisDServerAddress(bindAddress)
	}
	return strings.Join(kept, ",")
}

// thisDServerAddress returns the address a job dials to reach this dserver,
// which listens on bindAddress, its SSHBindAddress. The address has no port,
// so the job's client dials the configured SSHPort, the port dserver listens
// on (see clients.newClientRuntimeBoundary). A dserver listening on every
// address (bindAddress "", which used to give the --servers value "" and so
// failed every run, or the unspecified 0.0.0.0 or ::) is dialled at the
// loopback address of the same family: 127.0.0.1, or ::1 for :: (the
// IPv4-mapped ::ffff:0.0.0.0 counts as 0.0.0.0). On Linux a connection to
// the unspecified address reaches the loopback address anyway, so the job's
// remote IP, which its AllowFrom must permit, stays the same. Any other bind
// address is returned unchanged.
func thisDServerAddress(bindAddress string) string {
	if bindAddress == "" {
		return loopbackIPv4
	}
	bind, err := netip.ParseAddr(bindAddress)
	if err != nil || !bind.Unmap().IsUnspecified() {
		return bindAddress
	}
	if bind.Unmap().Is4() {
		return loopbackIPv4
	}
	return netip.IPv6Loopback().String()
}

// loopbackIPv4 is the IPv4 loopback address of thisDServerAddress.
const loopbackIPv4 = "127.0.0.1"

// logCreateFailure logs why the client of the job name could not be
// created. A discovery that yields no server (clients.ErrNoServers), e.g. a
// server list file with no entry yet, is a configuration problem the job
// retries at its next run, so it is logged at WARN, like a job that exited
// with a non-zero status; any other failure is logged at ERROR.
func logCreateFailure(logger logging.Logger, name string, err error) {
	message := fmt.Sprintf("Unable to create job %s", name)
	if errors.Is(err, clients.ErrNoServers) {
		logger.Warn(message, err)
		return
	}
	logger.Error(message, err)
}

func firstColorizer(colorizers []*brush.Brush) *brush.Brush {
	if len(colorizers) == 0 {
		return nil
	}
	return colorizers[0]
}

// Start runs scheduled and continuous workloads until the context is canceled.
func (r *Runner) Start(ctx context.Context) {
	if r == nil || r.scheduler == nil || r.continuous == nil {
		return
	}

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		r.scheduler.start(ctx)
	}()
	go func() {
		defer workers.Done()
		r.continuous.start(ctx)
	}()
	workers.Wait()
}
