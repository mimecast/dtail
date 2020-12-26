package clients

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/omode"
)

// CatClient is a client for returning a whole file from the beginning to the end.
type CatClient struct {
	baseClient
}

// NewCatClient returns a new cat client.
func NewCatClient(args Args) (*CatClient, error) {
	if args.RegexStr != "" {
		return nil, errors.New("Can't use regex with 'cat' operating mode")
	}

	args.Mode = omode.CatClient

	c := CatClient{
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

func (c CatClient) makeHandler(server string) handlers.Handler {
	return handlers.NewClientHandler(server)
}

func (c CatClient) makeCommands() (commands []string) {
	options := fmt.Sprintf("spartan=%v", c.Args.Spartan)
	for _, file := range strings.Split(c.What, ",") {
		commands = append(commands, fmt.Sprintf("%s:%s %s %s", c.Mode.String(), options, file, c.Regex.Serialize()))
	}
	return
}
