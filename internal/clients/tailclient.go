package clients

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"
)

// TailClient is used for tailing remote log files (opening, seeking to the end and returning only new incoming lines).
type TailClient struct {
	baseClient
}

// NewTailClient returns a new TailClient.
func NewTailClient(args Args) (*TailClient, error) {
	args.Mode = omode.TailClient

	c := TailClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      true,
		},
	}

	c.init()
	c.makeConnections(c)

	return &c, nil
}

func (c TailClient) makeHandler(server string) handlers.Handler {
	return handlers.NewClientHandler(server)
}

func (c TailClient) makeCommands() (commands []string) {
	options := fmt.Sprintf("spartan=%v", c.Args.Spartan)
	for _, file := range strings.Split(c.What, ",") {
		commands = append(commands, fmt.Sprintf("%s:%s %s %s", c.Mode.String(), options, file, c.Regex.Serialize()))
	}
	logger.Debug(commands)

	return
}
