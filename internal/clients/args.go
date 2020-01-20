package clients

import (
	"github.com/mimecast/dtail/internal/omode"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
	Mode              omode.Mode
	ServersStr        string
	UserName          string
	Files             string
	Regex             string
	TrustAllHosts     bool
	Discovery         string
	ConnectionsPerCPU int
	PingTimeout       int
}
