package client

import (
	"context"

	"golang.org/x/crypto/ssh"
)

// HostKeyCallback is a wrapper around ssh.KnownHosts so that we can add all
// unknown hosts in a single batch to the known_hosts file.
//
// Wrap returns an ssh.HostKeyCallback that is bound to the lifetime of the
// supplied SSH handshake context. Implementations MUST abort any blocking
// operations (e.g. prompting the user for unknown hosts) once ctx is
// cancelled so the SSH handshake does not hang and no goroutines leak.
type HostKeyCallback interface {
	Wrap(ctx context.Context) ssh.HostKeyCallback
	Untrusted(server string) bool
	PromptAddHosts(ctx context.Context)
}
