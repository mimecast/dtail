package clients

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/clients/remote"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

// ExecClient is a client for execute various commands on the server.
type ExecClient struct {
	baseClient
}

// NewExecClient returns a new cat client.
func NewExecClient(args Args) (*ExecClient, error) {
	args.Regex = "."
	args.Mode = omode.ExecClient

	c := ExecClient{
		baseClient: baseClient{
			Args:       args,
			stop:       make(chan struct{}),
			stopped:    make(chan struct{}),
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
	}

	c.init(c)

	return &c, nil
}

func (c ExecClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *remote.Connection {
	conn := remote.NewConnection(server, c.UserName, sshAuthMethods, hostKeyCallback)
	conn.Handler = handlers.NewClientHandler(server, c.PingTimeout)
	for _, file := range strings.Split(c.Files, ";") {
		conn.Commands = append(conn.Commands, fmt.Sprintf("%s %s", c.Mode.String(), file))
	}
	return conn
}
