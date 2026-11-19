package clients

import (
	"errors"
	"fmt"

	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// GrepClient searches a remote file for all lines matching a regular
// expression. Only the matching lines are displayed.
type GrepClient struct {
	baseClient
}

// NewGrepClient creates a new grep client.
func NewGrepClient(args config.Args, cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizers ...*brush.Brush) (*GrepClient, error) {
	if args.RegexStr == "" {
		return nil, errors.New("no regex specified, use '-regex' flag")
	}
	args.Mode = omode.GrepClient
	loggers = loggers.normalized()

	base, err := newBaseClient(args, cfg, loggers, firstColorizer(colorizers),
		clientHandlerProfile(loggers.Client, false))
	if err != nil {
		return nil, fmt.Errorf("initialize grep client: %w", err)
	}
	return &GrepClient{baseClient: base}, nil
}
