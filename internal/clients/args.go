package clients

import (
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// LineContext is here to help filtering out only specific lines.
type LineContext struct {
	RegexStr      string
	AfterContext  int
	BeforeContext int
	MaxCount      int
}

// Args is a helper struct to summarize common client arguments.
type Args struct {
	LineContext
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
