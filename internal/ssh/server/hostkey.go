package server

import (
	"io/ioutil"
	"os"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/ssh"
)

// PrivateHostKey retrieves the private server RSA host key.
func PrivateHostKey() []byte {
	hostKeyFile := config.Server.HostKeyFile
	if config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		hostKeyFile = "./ssh_host_key"
	}
	_, err := os.Stat(hostKeyFile)

	if os.IsNotExist(err) {
		dlog.Common.Info("Generating private server RSA host key")
		privateKey, err := ssh.GeneratePrivateRSAKey(config.Server.HostKeyBits)

		if err != nil {
			dlog.Common.FatalPanic("Failed to generate private server RSA host key", err)
		}

		pem := ssh.EncodePrivateKeyToPEM(privateKey)
		if err := ioutil.WriteFile(hostKeyFile, pem, 0600); err != nil {
			dlog.Common.Error("Unable to write private server RSA host key to file",
				hostKeyFile, err)
		}
		return pem
	}

	dlog.Common.Info("Reading private server RSA host key from file", hostKeyFile)
	pem, err := ioutil.ReadFile(hostKeyFile)
	if err != nil {
		dlog.Common.FatalPanic("Failed to load private server RSA host key", err)
	}
	return pem
}
