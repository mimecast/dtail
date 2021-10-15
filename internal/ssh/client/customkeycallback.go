package client

import (
	"net"

	"golang.org/x/crypto/ssh"
)

// CustomCallback is a custom host key callback wrapper.
type CustomCallback struct{}

// NewCustomCallback returns a new wrapper.
func NewCustomCallback() (*CustomCallback, error) {
	h := CustomCallback{}
	return &h, nil
}

// Wrap the host key callback.
func (h *CustomCallback) Wrap() ssh.HostKeyCallback {
	return func(server string, remote net.Addr, key ssh.PublicKey) error {
		return nil
	}
}
