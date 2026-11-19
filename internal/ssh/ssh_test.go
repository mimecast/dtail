package ssh

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestPrivateKeySignerLoadsUnencryptedKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "id_rsa")
	privateKey, keyErr := rsa.GenerateKey(rand.Reader, 1024)
	if keyErr != nil {
		t.Fatalf("GenerateKey failed: %v", keyErr)
	}

	if err := os.WriteFile(keyFile, EncodePrivateKeyToPEM(privateKey), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	signer, err := PrivateKeySigner(keyFile)
	if err != nil {
		t.Fatalf("PrivateKeySigner failed: %v", err)
	}
	if signer == nil {
		t.Fatalf("PrivateKeySigner returned nil signer")
	}
}

func TestPrivateKeySignerLoadsEncryptedKeyWithEnvPassphrase(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "id_rsa")
	privateKey, keyErr := rsa.GenerateKey(rand.Reader, 1024)
	if keyErr != nil {
		t.Fatalf("GenerateKey failed: %v", keyErr)
	}

	block, marshalErr := gossh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte("secret-passphrase"))
	if marshalErr != nil {
		t.Fatalf("MarshalPrivateKeyWithPassphrase failed: %v", marshalErr)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	t.Setenv("DTAIL_KEY_PASSPHRASE", "secret-passphrase")

	signer, err := PrivateKeySigner(keyFile)
	if err != nil {
		t.Fatalf("PrivateKeySigner failed: %v", err)
	}
	if signer == nil {
		t.Fatalf("PrivateKeySigner returned nil signer")
	}
}

func TestPrivateKeySignerReturnsPassphraseMissingWithoutEnv(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "id_rsa")
	privateKey, keyErr := rsa.GenerateKey(rand.Reader, 1024)
	if keyErr != nil {
		t.Fatalf("GenerateKey failed: %v", keyErr)
	}

	block, marshalErr := gossh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte("secret-passphrase"))
	if marshalErr != nil {
		t.Fatalf("MarshalPrivateKeyWithPassphrase failed: %v", marshalErr)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := PrivateKeySigner(keyFile)
	if err == nil {
		t.Fatalf("PrivateKeySigner succeeded without passphrase env")
	}

	var passphraseMissingErr *gossh.PassphraseMissingError
	if !errors.As(err, &passphraseMissingErr) {
		t.Fatalf("PrivateKeySigner returned %T, want PassphraseMissingError", err)
	}
}

func TestPrivateKeySignerRejectsIncorrectEnvPassphrase(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "id_rsa")
	privateKey, keyErr := rsa.GenerateKey(rand.Reader, 1024)
	if keyErr != nil {
		t.Fatalf("GenerateKey failed: %v", keyErr)
	}

	block, marshalErr := gossh.MarshalPrivateKeyWithPassphrase(privateKey, "", []byte("secret-passphrase"))
	if marshalErr != nil {
		t.Fatalf("MarshalPrivateKeyWithPassphrase failed: %v", marshalErr)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	t.Setenv("DTAIL_KEY_PASSPHRASE", "wrong-passphrase")

	_, err := PrivateKeySigner(keyFile)
	if err == nil {
		t.Fatalf("PrivateKeySigner succeeded with wrong passphrase env")
	}

	var passphraseMissingErr *gossh.PassphraseMissingError
	if errors.As(err, &passphraseMissingErr) {
		t.Fatalf("PrivateKeySigner returned PassphraseMissingError instead of parse failure")
	}
}
