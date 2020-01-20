package clients

import (
	"github.com/mimecast/dtail/internal/clients/remote"
	"github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

type connectionMaker interface {
	makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *remote.Connection
}
