package ssh

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"syscall"

	"github.com/mimecast/dtail/internal/io/dlog"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/terminal"
)

// GeneratePrivateRSAKey is used by the server to generate its key.
func GeneratePrivateRSAKey(size int) (*rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, size)
	if err != nil {
		return nil, err
	}
	err = privateKey.Validate()
	if err != nil {
		return nil, err
	}
	return privateKey, nil
}

// EncodePrivateKeyToPEM is a helper function for converting a key to PEM format.
func EncodePrivateKeyToPEM(privateKey *rsa.PrivateKey) []byte {
	derFormat := x509.MarshalPKCS1PrivateKey(privateKey)

	block := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   derFormat,
	}
	return pem.EncodeToMemory(&block)
}

// Agent used for SSH auth.
func Agent() (gossh.AuthMethod, error) {
	sshAgent, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, err
	}
	agentClient := agent.NewClient(sshAgent)
	keys, err := agentClient.List()
	if err != nil {
		return nil, err
	}
	for i, key := range keys {
		dlog.Common.Debug("Public key", i, key)
	}
	return gossh.PublicKeysCallback(agentClient.Signers), nil
}

// EnterKeyPhrase is required to read phrase protected private keys.
func EnterKeyPhrase(keyFile string) []byte {
	fmt.Printf("Enter phrase for key %s: ", keyFile)
	phrase, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", string(phrase))
	return phrase
}

// KeyFile returns the key as a SSH auth method.
func KeyFile(keyFile string) (gossh.AuthMethod, error) {
	buffer, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	key, err := gossh.ParsePrivateKey(buffer)
	if err != nil {
		return nil, err
	}

	// Key phrase support disabled as password will be printed to stdout!
	/*
		if err == nil {
			return gossh.PublicKeys(key), nil
		}

		keyPhrase := EnterKeyPhrase(keyFile)
		key, err = gossh.ParsePrivateKeyWithPassphrase(buffer, keyPhrase)
		if err != nil {
			return nil, err
		}
	*/

	return gossh.PublicKeys(key), nil
}

// PrivateKey returns the private key as a SSH auth method.
func PrivateKey(keyFile string) (gossh.AuthMethod, error) {
	signer, err := KeyFile(keyFile)
	if err != nil {
		dlog.Common.Debug(keyFile, err)
		return nil, err
	}
	return gossh.AuthMethod(signer), nil
}
