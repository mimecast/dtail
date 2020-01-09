package clients

import (
	"dtail/clients/remote"
	"dtail/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

type connectionMaker interface {
	makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *remote.Connection
}
