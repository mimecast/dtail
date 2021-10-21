package client

import (
	"os"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/ssh"

	gossh "golang.org/x/crypto/ssh"
)

// InitSSHAuthMethods initialises all known SSH auth methods on the client side.
func InitSSHAuthMethods(sshAuthMethods []gossh.AuthMethod,
	hostKeyCallback gossh.HostKeyCallback, trustAllHosts bool, throttleCh chan struct{},
	privateKeyPath string) ([]gossh.AuthMethod, HostKeyCallback) {

	if len(sshAuthMethods) > 0 {
		simpleCallback, err := NewSimpleCallback()
		if err != nil {
			dlog.Client.FatalPanic(err)
		}
		return sshAuthMethods, simpleCallback
	}
	return initKnownHostsAuthMethods(trustAllHosts, throttleCh, privateKeyPath)
}

func initKnownHostsAuthMethods(trustAllHosts bool, throttleCh chan struct{},
	privateKeyPath string) ([]gossh.AuthMethod, HostKeyCallback) {

	var sshAuthMethods []gossh.AuthMethod
	knownHostsPath := os.Getenv("HOME") + "/.ssh/known_hosts"
	knownHostsCallback, err := NewKnownHostsCallback(knownHostsPath, trustAllHosts,
		throttleCh)
	if err != nil {
		dlog.Client.FatalPanic(knownHostsPath, err)
	}
	dlog.Client.Debug("initKnownHostsAuthMethods", "Added known hosts file path", knownHostsPath)
	/*
		if config.Client.ExperimentalFeaturesEnable {
			sshAuthMethods = append(sshAuthMethods, gossh.Password("experimental feature test"))
			dlog.Client.Debug("initKnownHostsAuthMethods", "Added experimental method to list of auth methods")
		}
	*/

	// First try to read custom private key path.
	if privateKeyPath != "" {
		authMethod, err := ssh.PrivateKey(privateKeyPath)
		if err == nil {
			sshAuthMethods = append(sshAuthMethods, authMethod)
			dlog.Client.Debug("initKnownHostsAuthMethods",
				"Added path to list of auth methods, not adding further methods",
				privateKeyPath)
			return sshAuthMethods, knownHostsCallback
		}
		dlog.Client.FatalPanic("Unable to use private SSH key", privateKeyPath, err)
	}

	// Second, try SSH Agent
	authMethod, err := ssh.Agent()
	if err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		dlog.Client.Debug("initKnownHostsAuthMethods", "Added SSH Agent (SSH_AUTH_SOCK)"+
			"to list of auth methods, not adding further methods")
		return sshAuthMethods, knownHostsCallback
	}
	dlog.Client.Debug("initKnownHostsAuthMethods",
		"Unable to init SSH Agent auth method", err)

	// Third, try Linux/UNIX default key paths
	privateKeyPath = os.Getenv("HOME") + "/.ssh/id_rsa"
	authMethod, err = ssh.PrivateKey(privateKeyPath)
	if err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		dlog.Client.Debug("initKnownHostsAuthmethods",
			"Added path to list of auth methods, not adding further methods", privateKeyPath)
		return sshAuthMethods, knownHostsCallback
	}
	dlog.Client.Debug("initKnownHostsAuthMethods", "Unable to use private key",
		privateKeyPath, err)

	privateKeyPath = os.Getenv("HOME") + "/.ssh/id_dsa"
	authMethod, err = ssh.PrivateKey(privateKeyPath)
	if err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		dlog.Client.Debug("initKnownHostsAuthmethods",
			"Added path to list of auth methods, not adding further methods", privateKeyPath)
		return sshAuthMethods, knownHostsCallback
	}

	privateKeyPath = os.Getenv("HOME") + "/.ssh/id_ecdsa"
	authMethod, err = ssh.PrivateKey(privateKeyPath)
	if err == nil {
		sshAuthMethods = append(sshAuthMethods, authMethod)
		dlog.Client.Debug("initKnownHostsAuthmethods",
			"Added path to list of auth methods, not adding further methods", privateKeyPath)
		return sshAuthMethods, knownHostsCallback
	}

	dlog.Client.Debug("initKnownHostsAuthMethods", "Unable to use private key",
		privateKeyPath, err)

	// This is only a panic when we expect to do something about it.
	if !config.Client.SSHDontAddHostsToKnownHostsFile {
		dlog.Client.FatalPanic("Unable to find private SSH key information")
	}

	// Never reach this point.
	return sshAuthMethods, knownHostsCallback
}
