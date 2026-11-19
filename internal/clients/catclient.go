package clients

import (
	"errors"
	"fmt"

	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// CatClient is a client for returning a whole file from the beginning to the end.
type CatClient struct {
	baseClient
}

// NewCatClient returns a new cat client.
func NewCatClient(args config.Args, cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizers ...*brush.Brush) (*CatClient, error) {
	if args.RegexStr != "" {
		return nil, errors.New("can't use regex with 'cat' operating mode")
	}
	args.Mode = omode.CatClient
	loggers = loggers.normalized()

	base, err := newBaseClient(args, cfg, loggers, firstColorizer(colorizers),
		clientHandlerProfile(loggers.Client, false))
	if err != nil {
		return nil, fmt.Errorf("initialize cat client: %w", err)
	}
	return &CatClient{baseClient: base}, nil
}
