package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
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
}

// When no servers are given, connect to localhost!
func (a *Args) handleEmptyServer() {
	if a.Discovery != "" || a.ServersStr != "" {
		return
	}

	fqdn, err := os.Hostname()
	if err != nil {
		logger.FatalExit(err)
	}
	a.ServersStr = fmt.Sprintf("%s:%d", fqdn, config.Common.SSHPort)
	// I am trusting my own hostname.
	a.TrustAllHosts = true
	logger.Debug("Will connect to local server", a.ServersStr)

	cleanPath := func(dirtyPath string) string {
		cleanPath, err := filepath.EvalSymlinks(dirtyPath)
		if err != nil {
			logger.FatalExit("Unable to evaluate symlinks", dirtyPath, err)
		}
		cleanPath, err = filepath.Abs(cleanPath)
		if err != nil {
			logger.FatalExit("Unable to make file path absolute", dirtyPath, cleanPath, err)
		}
		return cleanPath
	}

	logger.Debug("Dirty file paths", a.What)
	var filePaths []string
	for _, dirtyPath := range strings.Split(a.What, ",") {
		filePaths = append(filePaths, cleanPath(dirtyPath))
	}

	a.What = strings.Join(filePaths, ",")
	logger.Debug("Clean file paths", a.What)
}
