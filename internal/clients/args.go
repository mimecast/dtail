package clients

import (
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
	Mode               omode.Mode
	ServersStr         string
	UserName           string
	What               string
	Arguments          []string
	Regex              string
	TrustAllHosts      bool
	Discovery          string
	ConnectionsPerCPU  int
	Timeout            int
	SSHAuthMethods     []gossh.AuthMethod
	SSHHostKeyCallback gossh.HostKeyCallback
}
