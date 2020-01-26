package server

import (
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/ssh"
	"io/ioutil"
	"os"
)

// PrivateHostKey retrieves the private server RSA host key.
func PrivateHostKey() []byte {
	hostKeyFile := config.Server.HostKeyFile
	_, err := os.Stat(hostKeyFile)

	if os.IsNotExist(err) {
		logger.Info("Generating private server RSA host key")
		privateKey, err := ssh.GeneratePrivateRSAKey(config.Server.HostKeyBits)

		if err != nil {
			logger.FatalExit("Failed to generate private server RSA host key", err)
		}

		pem := ssh.EncodePrivateKeyToPEM(privateKey)
		if err := ioutil.WriteFile(hostKeyFile, pem, 0600); err != nil {
			logger.Error("Unable to write private server RSA host key to file", hostKeyFile, err)
		}
		return pem
	}

	logger.Info("Reading private server RSA host key from file", hostKeyFile)
	pem, err := ioutil.ReadFile(hostKeyFile)
	if err != nil {
		logger.FatalExit("Failed to load private server RSA host key", err)
	}
	return pem
}
