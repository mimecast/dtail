package server

import (
	"errors"
	"fmt"
	iofs "io/fs"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/ssh"
)

const (
	defaultHostKeyBits = 4096
	defaultHostKeyFile = "./cache/ssh_host_key"
)

// PrivateHostKey retrieves the private server RSA host key.
func PrivateHostKey(hostKeyFile string, hostKeyBits int, logger logging.Logger) ([]byte, error) {
	logger = logging.OrNop(logger)
	if hostKeyFile == "" {
		hostKeyFile = defaultHostKeyFile
	}
	if hostKeyBits <= 0 {
		hostKeyBits = defaultHostKeyBits
	}
	hostKeyPath, err := fs.NewRootedPath(hostKeyFile)
	if err != nil {
		return nil, fmt.Errorf("invalid private server RSA host key path %q: %w", hostKeyFile, err)
	}

	_, err = hostKeyPath.Stat()
	if err != nil {
		// os.IsNotExist does not unwrap fmt.Errorf chains from RootedPath.Stat; use errors.Is.
		if errors.Is(err, iofs.ErrNotExist) {
			logger.Info("Generating private server RSA host key")
			pem, genErr := generatePrivateHostKey(hostKeyBits)
			if genErr != nil {
				return nil, fmt.Errorf("generate private server RSA host key: %w", genErr)
			}
			if storeErr := storePrivateHostKey(hostKeyPath, pem); storeErr != nil {
				logger.Error("Unable to write private server RSA host key to file",
					hostKeyFile, storeErr)
			}
			return pem, nil
		}
		return nil, fmt.Errorf("stat private server RSA host key path %q: %w", hostKeyFile, err)
	}

	logger.Info("Reading private server RSA host key from file", hostKeyFile)
	pem, err := readPrivateHostKey(hostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load private server RSA host key from %q: %w", hostKeyFile, err)
	}
	return pem, nil
}

func generatePrivateHostKey(hostKeyBits int) ([]byte, error) {
	privateKey, err := ssh.GeneratePrivateRSAKey(hostKeyBits)
	if err != nil {
		return nil, err
	}

	return ssh.EncodePrivateKeyToPEM(privateKey), nil
}

func storePrivateHostKey(hostKeyPath fs.RootedPath, pem []byte) error {
	return hostKeyPath.WriteFile(pem, 0o600)
}

func readPrivateHostKey(hostKeyPath fs.RootedPath) ([]byte, error) {
	return hostKeyPath.ReadFile()
}
