package clients

import (
	"dtail/clients/handlers"
	"dtail/clients/remote"
	"dtail/ssh/client"
	"fmt"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

// TailClient is used for tailing remote log files (opening, seeking to the end and returning only new incoming lines).
type TailClient struct {
	baseClient
}

// NewTailClient returns a new TailClient.
func NewTailClient(args Args) (*TailClient, error) {
	c := TailClient{
		baseClient: baseClient{
			Args:       args,
			stop:       make(chan struct{}),
			stopped:    make(chan struct{}),
			throttleCh: make(chan struct{}, args.MaxInitConnections),
			retry:      true,
		},
	}

	c.init(c)

	return &c, nil
}

func (c TailClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *remote.Connection {
	conn := remote.NewConnection(server, c.UserName, sshAuthMethods, hostKeyCallback)
	conn.Handler = handlers.NewClientHandler(server, c.PingTimeout)

	for _, file := range strings.Split(c.Files, ",") {
		conn.Commands = append(conn.Commands, fmt.Sprintf("%s %s regex %s", c.Mode.String(), file, c.Regex))
	}

	return conn
}
