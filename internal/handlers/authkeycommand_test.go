package handlers

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	userserver "github.com/mimecast/dtail/internal/sessionuser"

	gossh "golang.org/x/crypto/ssh"
)

func TestHandleAuthKeyCommandSuccess(t *testing.T) {
	handler := newAuthKeyTestHandler("authkey-success-user", true)
	key := handlerTestPublicKey(t, 31)
	keyArg := base64.StdEncoding.EncodeToString(key.Marshal())

	commandFinished := false
	handler.handleAuthKeyCommand(context.Background(), lcontext.LContext{}, 2,
		[]string{"AUTHKEY", keyArg}, func() {
			commandFinished = true
		})

	if !commandFinished {
		t.Fatalf("Expected commandFinished callback to be called")
	}
	if message := readServerMessage(t, handler.serverMessages); message != "AUTHKEY OK\n" {
		t.Fatalf("Unexpected response: %q", message)
	}
	if !handler.authKeyStore.Has(handler.user.Name, key) {
		t.Fatalf("Expected key to be stored for user")
	}
	handler.authKeyStore.Remove(handler.user.Name, key)
}

func TestHandleAuthKeyCommandFeatureDisabled(t *testing.T) {
	handler := newAuthKeyTestHandler("authkey-disabled-user", false)
	key := handlerTestPublicKey(t, 32)
	keyArg := base64.StdEncoding.EncodeToString(key.Marshal())

	handler.handleAuthKeyCommand(context.Background(), lcontext.LContext{}, 2,
		[]string{"AUTHKEY", keyArg}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != "AUTHKEY ERR feature disabled\n" {
		t.Fatalf("Unexpected response: %q", message)
	}
	if handler.authKeyStore.Has(handler.user.Name, key) {
		t.Fatalf("Expected no key to be stored while feature is disabled")
	}
}

func TestHandleAuthKeyCommandRejectsPasswordOnlyUsers(t *testing.T) {
	key := handlerTestPublicKey(t, 33)
	keyArg := base64.StdEncoding.EncodeToString(key.Marshal())

	tests := []struct {
		name     string
		userName string
	}{
		{name: "health", userName: config.HealthUser},
		{name: "schedule", userName: config.ScheduleUser},
		{name: "continuous", userName: config.ContinuousUser},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newAuthKeyTestHandler(tt.userName, true)

			handler.handleAuthKeyCommand(context.Background(), lcontext.LContext{}, 2,
				[]string{"AUTHKEY", keyArg}, func() {})

			if message := readServerMessage(t, handler.serverMessages); message != "AUTHKEY ERR unsupported user\n" {
				t.Fatalf("Unexpected response: %q", message)
			}
			if handler.authKeyStore.Has(tt.userName, key) {
				t.Fatal("Expected no key to be stored for password-only user")
			}
		})
	}
}

func TestHandleAuthKeyCommandInvalidPayload(t *testing.T) {
	handler := newAuthKeyTestHandler("authkey-invalid-user", true)

	handler.handleAuthKeyCommand(context.Background(), lcontext.LContext{}, 2,
		[]string{"AUTHKEY", "not-base64"}, func() {})

	if message := readServerMessage(t, handler.serverMessages); message != "AUTHKEY ERR invalid base64\n" {
		t.Fatalf("Unexpected response for invalid base64: %q", message)
	}

	validButNonSSH := base64.StdEncoding.EncodeToString([]byte("not-an-ssh-key"))
	handler.handleAuthKeyCommand(context.Background(), lcontext.LContext{}, 2,
		[]string{"AUTHKEY", validButNonSSH}, func() {})
	if message := readServerMessage(t, handler.serverMessages); message != "AUTHKEY ERR invalid public key\n" {
		t.Fatalf("Unexpected response for invalid key bytes: %q", message)
	}
}

func newAuthKeyTestHandler(userName string, authKeyEnabled bool) *ServerHandler {
	return &ServerHandler{
		baseHandler: newBaseHandler(context.Background(), baseHandlerConfig{
			serverMessages: make(chan string, 4),
			user:           &userserver.User{Name: userName},
		}),
		serverCfg: &config.ServerConfig{
			AuthKeyEnabled: authKeyEnabled,
		},
		authKeyStore: authkey.New(time.Hour, 5),
	}
}

func handlerTestPublicKey(t *testing.T, seedByte byte) gossh.PublicKey {
	t.Helper()

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}

	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey, err := gossh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatalf("Unable to build ssh public key: %s", err.Error())
	}

	return publicKey
}

func readServerMessage(t *testing.T, messages <-chan string) string {
	t.Helper()

	select {
	case message := <-messages:
		return message
	case <-time.After(time.Second):
		t.Fatalf("Timed out waiting for server message")
		return ""
	}
}
