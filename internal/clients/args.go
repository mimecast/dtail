package clients

import (
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
	lcontext.LContext
	RegexStr           string
	Mode               omode.Mode
	ServersStr         string
	UserName           string
	What               string
	Arguments          []string
	RegexInvert        bool
	TrustAllHosts      bool
	Discovery          string
	ConnectionsPerCPU  int
	Timeout            int
	SSHAuthMethods     []gossh.AuthMethod
	SSHHostKeyCallback gossh.HostKeyCallback
	PrivateKeyPathFile string
	Quiet              bool
}
