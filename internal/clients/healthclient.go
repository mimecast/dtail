package clients

import (
	"context"
	"fmt"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// HealthClient is used to perform a basic server health check.
type HealthClient struct {
	baseClient
}

// NewHealthClient returns a new health client.
func NewHealthClient(args config.Args, cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizers ...*brush.Brush) (*HealthClient, error) {
	args.Mode = omode.HealthClient
	args.UserName = config.HealthUser
	args.SSHAuthMethods = append(args.SSHAuthMethods, gossh.Password(config.HealthUser))
	loggers = loggers.normalized()

	base, err := newBaseClient(args, cfg, loggers, firstColorizer(colorizers), clientProfile{
		newHandler: func(server string) handlers.Handler {
			return handlers.NewHealthHandler(server, loggers.Client)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize health client: %w", err)
	}
	return &HealthClient{baseClient: base}, nil
}

// Start the health client.
func (c *HealthClient) Start(ctx context.Context, statsCh <-chan string) int {
	status := c.baseClient.Start(ctx, statsCh)

	switch status {
	case 0:
		if c.Serverless {
			fmt.Printf("WARNING: All seems fine but the check only run in serverless mode" +
				", please specify a remote server via --server hostname:port\n")
			return 1
		}
		fmt.Printf("OK: All fine at %s :-)\n", c.ServersStr)
	case 2:
		if c.Serverless {
			fmt.Printf("CRITICAL: DTail server not operating properly (using " +
				"serverless connction)!\n")
			return 2
		}
		fmt.Printf("CRITICAL: DTail server not operating properly at %s!\n",
			c.ServersStr)
	default:
		if c.Serverless {
			fmt.Printf("UNKNOWN: Received unknown status code %d (using serverless "+
				"connection)\n", status)
			return status
		}
		fmt.Printf("UNKNOWN: Received unknown status code %d from %s!\n",
			status, c.ServersStr)
	}

	return status
}
