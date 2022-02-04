package client

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/mimecast/dtail/internal/io/dlog"
	"golang.org/x/crypto/ssh"
)

// GeneratePrivatePublicKeyPairIfNotExists generates a SSH key pair (used by the integration tests)
func GeneratePrivatePublicKeyPairIfNotExists(keyPath string, bitSize int) {
	if _, err := os.Stat(keyPath); err == nil {
		dlog.Client.Debug("Private/public key pair already exists", keyPath)
		return
	}
	GeneratePrivatePublicKeyPair(keyPath, bitSize)
}

// GeneratePrivatePublicKeyPair generates a SSH key pair (used by the integration tests)
func GeneratePrivatePublicKeyPair(keyPath string, bitSize int) {
	privateKeyPath := keyPath
	publicKeyPath := fmt.Sprintf("%s.pub", keyPath)

	dlog.Client.Debug("Generating private/public key pair", privateKeyPath, publicKeyPath)

	privateKey, err := generatePrivateKey(bitSize)
	if err != nil {
		dlog.Client.FatalPanic(err)
	}
	publicKeyBytes, err := generatePublicKey(&privateKey.PublicKey)
	if err != nil {
		dlog.Client.FatalPanic(err)
	}
	privateKeyBytes := encodePrivateKeyToPEM(privateKey)
	err = writeKey(privateKeyBytes, privateKeyPath)
	if err != nil {
		dlog.Client.FatalPanic(err)
	}
	err = writeKey([]byte(publicKeyBytes), publicKeyPath)
	if err != nil {
		dlog.Client.FatalPanic(err)
	}

	dlog.Client.Debug("Done generating private/public key pair", privateKeyPath, publicKeyPath)
}

func generatePrivateKey(bitSize int) (*rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, bitSize)
	if err != nil {
		return nil, err
	}
	err = privateKey.Validate()
	if err != nil {
		return nil, err
	}
	return privateKey, nil
}

func encodePrivateKeyToPEM(privateKey *rsa.PrivateKey) []byte {
	privDER := x509.MarshalPKCS1PrivateKey(privateKey)
	privBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   privDER,
	}
	privatePEM := pem.EncodeToMemory(&privBlock)
	return privatePEM
}

func generatePublicKey(privatekey *rsa.PublicKey) ([]byte, error) {
	publicRsaKey, err := ssh.NewPublicKey(privatekey)
	if err != nil {
		return nil, err
	}
	pubKeyBytes := ssh.MarshalAuthorizedKey(publicRsaKey)
	return pubKeyBytes, nil
}

func writeKey(keyBytes []byte, saveFileTo string) error {
	err := ioutil.WriteFile(saveFileTo, keyBytes, 0600)
	if err != nil {
		return err
	}
	return nil
}
