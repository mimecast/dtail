package clients

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/omode"
)

// GrepClient searches a remote file for all lines matching a regular
// expression. Only the matching lines are displayed.
type GrepClient struct {
	baseClient
}

// NewGrepClient creates a new grep client.
func NewGrepClient(args config.Args) (*GrepClient, error) {
	if args.RegexStr == "" {
		return nil, errors.New("No regex specified, use '-regex' flag")
	}
	args.Mode = omode.GrepClient

	c := GrepClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
	}

	c.init()
	c.makeConnections(c)
	return &c, nil
}

func (c GrepClient) makeHandler(server string) handlers.Handler {
	return handlers.NewClientHandler(server)
}

func (c GrepClient) makeCommands() (commands []string) {
	regex, err := c.Regex.Serialize()
	if err != nil {
		dlog.Client.FatalPanic(err)
	}
	for _, file := range strings.Split(c.What, ",") {
		commands = append(commands, fmt.Sprintf("%s:%s %s %s",
			c.Mode.String(), c.Args.SerializeOptions(), file, regex))
	}
	return
}
