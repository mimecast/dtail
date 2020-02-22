package clients

import (
	"fmt"
	"runtime"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/omode"
)

// RunClient is a client to run various commands on the server.
type RunClient struct {
	baseClient
	background bool
	cancel     bool
}

// NewRunClient returns a new cat client.
func NewRunClient(args Args, background, cancel bool) (*RunClient, error) {
	args.Mode = omode.RunClient

	c := RunClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
		background: background,
		cancel:     cancel,
	}

	c.init(c)
	return &c, nil
}

func (c RunClient) makeHandler(server string) handlers.Handler {
	return handlers.NewClientHandler(server)
}

func (c RunClient) makeCommands() (commands []string) {
	if c.Timeout > 0 {
		commands = append(commands, fmt.Sprintf("timeout %d run%s %s", c.Timeout, c.flags(), c.What))
		return
	}

	commands = append(commands, fmt.Sprintf("run%s %s", c.flags(), c.What))
	return
}

func (c RunClient) flags() string {
	if c.background {
		return ":background.start"
	}
	if c.cancel {
		return ":background.cancel"
	}
	return ""
}
