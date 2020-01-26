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
}

// NewRunClient returns a new cat client.
func NewRunClient(args Args) (*RunClient, error) {
	args.Mode = omode.RunClient

	c := RunClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
	}

	c.init(c)
	return &c, nil
}

func (c RunClient) makeHandler(server string) handlers.Handler {
	return handlers.NewClientHandler(server)
}

func (c RunClient) makeCommands() (commands []string) {
	// Send "run COMMAND" to server!
	commands = append(commands, fmt.Sprintf("%s %s", c.Mode.String(), c.What))
	return
}
