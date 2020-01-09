package clients

import (
	"dtail/clients/handlers"
	"dtail/clients/remote"
	"dtail/ssh/client"
	"errors"
	"fmt"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

// GrepClient searches a remote file for all lines matching a regular expression. Only the matching lines are displayed.
type GrepClient struct {
	baseClient
}

// NewGrepClient creates a new grep client.
func NewGrepClient(args Args) (*GrepClient, error) {
	if args.Regex == "" {
		return nil, errors.New("No regex specified, use '-regex' flag")
	}

	c := GrepClient{
		baseClient: baseClient{
			Args:       args,
			stop:       make(chan struct{}),
			stopped:    make(chan struct{}),
			throttleCh: make(chan struct{}, args.MaxInitConnections),
			retry:      false,
		},
	}

	c.init(c)

	return &c, nil
}

func (c GrepClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *remote.Connection {
	conn := remote.NewConnection(server, c.UserName, sshAuthMethods, hostKeyCallback)
	conn.Handler = handlers.NewClientHandler(server, c.PingTimeout)

	for _, file := range strings.Split(c.Files, ",") {
		conn.Commands = append(conn.Commands, fmt.Sprintf("%s %s regex %s", c.Mode.String(), file, c.Regex))
	}

	return conn
}
