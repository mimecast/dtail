package clients

import (
	"flag"
	"fmt"
	"strings"

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
	RegexStr           string
	RegexInvert        bool
	TrustAllHosts      bool
	Discovery          string
	ConnectionsPerCPU  int
	Timeout            int
	SSHAuthMethods     []gossh.AuthMethod
	SSHHostKeyCallback gossh.HostKeyCallback
	PrivateKeyPathFile string
	Quiet              bool
	Spartan            bool
	NoColor            bool
	Serverless         bool
}

// Transform the arguments based on certain conditions.
func (a *Args) Transform(args []string) {
	// Interpret additional args as file list.
	if a.What == "" {
		var files []string
		for _, file := range flag.Args() {
			files = append(files, file)
		}
		a.What = strings.Join(files, ",")
	}

	if a.Spartan {
		a.Quiet = true
		a.NoColor = true
	}

	if a.Discovery == "" && a.ServersStr == "" {
		a.Serverless = true
	}
}

// TransformAfterConfigFile same as Transform, but after the config file has been read.
func (a *Args) TransformAfterConfigFile() {
	// TODO: Remove this method. It's not used.
}

func (a *Args) SerializeOptions() string {
	return fmt.Sprintf("quiet=%v:spartan=%v", a.Quiet, a.Spartan)
}
