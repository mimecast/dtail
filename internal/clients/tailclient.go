package clients

import (
	"fmt"

	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// TailClient is used for tailing remote log files (opening, seeking to the end and returning only new incoming lines).
type TailClient struct {
	baseClient
}

// NewTailClient returns a new TailClient.
func NewTailClient(args config.Args, cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizers ...*brush.Brush) (*TailClient, error) {
	args.Mode = omode.TailClient
	loggers = loggers.normalized()
	base, err := newBaseClient(args, cfg, loggers, firstColorizer(colorizers),
		clientHandlerProfile(loggers.Client, true))
	if err != nil {
		return nil, fmt.Errorf("initialize tail client: %w", err)
	}
	return &TailClient{baseClient: base}, nil
}
