package client

import (
	"os"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/ssh"

	gossh "golang.org/x/crypto/ssh"
)

// InitSSHAuthMethods initialises all known SSH auth methods on the client side.
func InitSSHAuthMethods(sshAuthMethods []gossh.AuthMethod, hostKeyCallback gossh.HostKeyCallback, trustAllHosts bool, throttleCh chan struct{}) ([]gossh.AuthMethod, HostKeyCallback) {
	if len(sshAuthMethods) > 0 {
		simpleCallback, err := NewSimpleCallback()
		if err != nil {
			logger.FatalExit(err)
		}
		return sshAuthMethods, simpleCallback
	}

	return initKnownHostsAuthMethods(trustAllHosts, throttleCh)
}

func initKnownHostsAuthMethods(trustAllHosts bool, throttleCh chan struct{}) ([]gossh.AuthMethod, HostKeyCallback) {
	var sshAuthMethods []gossh.AuthMethod
	if config.Common.ExperimentalFeaturesEnable {
		sshAuthMethods = append(sshAuthMethods, gossh.Password("experimental feature test"))
		logger.Debug("Added experimental method to list of auth methods")
	}

	keyPath := os.Getenv("HOME") + "/.ssh/id_rsa"
	if authMethod, err := ssh.PrivateKey(keyPath); err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		logger.Debug("Added path to list of auth methods", keyPath)
	}

	keyPath = os.Getenv("HOME") + "/.ssh/id_dsa"
	if authMethod, err := ssh.PrivateKey(keyPath); err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		logger.Debug("Added path to list of auth methods", keyPath)
	}

	if authMethod, err := ssh.Agent(); err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		logger.Debug("Added SSH Agent to list of auth methods")
	}

	knownHostsPath := os.Getenv("HOME") + "/.ssh/known_hosts"
	knownHostsCallback, err := NewKnownHostsCallback(knownHostsPath, trustAllHosts, throttleCh)
	if err != nil {
		logger.FatalExit(knownHostsPath, err)
	}
	logger.Debug("Added known hosts file path", knownHostsPath)

	return sshAuthMethods, knownHostsCallback
}
