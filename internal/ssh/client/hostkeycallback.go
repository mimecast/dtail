package client

import (
	"context"

	"golang.org/x/crypto/ssh"
)

// HostKeyCallback is a wrapper around ssh.KnownHosts so that we can add all
// unknown hosts in a single batch to the known_hosts file.
type HostKeyCallback interface {
	Wrap() ssh.HostKeyCallback
	Untrusted(server string) bool
	PromptAddHosts(ctx context.Context)
}
