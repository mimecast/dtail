package clients

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/clients/remote"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

// CatClient is a client for returning a whole file from the beginning to the end.
type CatClient struct {
	baseClient
}

// NewCatClient returns a new cat client.
func NewCatClient(args Args) (*CatClient, error) {
	if args.Regex != "" {
		return nil, errors.New("Can't use regex with 'cat' operating mode")
	}

	args.Regex = "."
	args.Mode = omode.CatClient

	c := CatClient{
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

func (c CatClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback *client.HostKeyCallback) *remote.Connection {
	conn := remote.NewConnection(server, c.UserName, sshAuthMethods, hostKeyCallback)
	conn.Handler = handlers.NewClientHandler(server, c.PingTimeout)
	for _, file := range strings.Split(c.Files, ",") {
		conn.Commands = append(conn.Commands, fmt.Sprintf("%s %s regex %s", c.Mode.String(), file, c.Regex))
	}
	return conn
}
