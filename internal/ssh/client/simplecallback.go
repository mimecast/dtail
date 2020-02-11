package client

import (
	"context"
	"net"

	"golang.org/x/crypto/ssh"
)

// SimpleCallback is a wrapper around ssh.KnownHosts so that we can add all
// unknown hosts in a single batch to the known_hosts file.
type SimpleCallback struct {
}

// NewSimpleCallback returns a new wrapper.
func NewSimpleCallback() (SimpleCallback, error) {
	return SimpleCallback{}, nil
}

// Wrap the host key callback.
func (SimpleCallback) Wrap() ssh.HostKeyCallback {
	return func(server string, remote net.Addr, key ssh.PublicKey) error {
		return nil
	}
}

// Untrusted returns whether host is not trusted or not.
func (SimpleCallback) Untrusted(server string) bool {
	return false
}

// PromptAddHosts prompts a question to the user whether unknown hosts should
// be added to the known hosts or not.
func (SimpleCallback) PromptAddHosts(ctx context.Context) {
	// Not used here.
}
