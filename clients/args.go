package clients

import (
	"dtail/omode"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
	// The operating mode (tail, grep, ...)
	Mode omode.Mode
	// The raw server string
	ServersStr string
	// SSH user name (e.g. 'pbuetow')
	UserName string
	// The files to follow.
	Files string
	// Regex for filtering.
	Regex string
	// Trust all unknown host keys?
	TrustAllHosts bool
	// Server discovery method
	Discovery          string
	MaxInitConnections int
	// Server ping timeout (0 means pings disabled)
	PingTimeout int
}
