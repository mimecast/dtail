package clients

import (
	"context"
	"fmt"
	"runtime"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// HealthClient is used to perform a basic server health check.
type HealthClient struct {
	baseClient
}

// NewHealthClient returns a new health client.
func NewHealthClient(args config.Args) (*HealthClient, error) {
	args.Mode = omode.HealthClient
	args.UserName = config.HealthUser
	c := HealthClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
	}

	c.init()
	c.sshAuthMethods = append(c.sshAuthMethods, gossh.Password(config.HealthUser))
	c.makeConnections(c)

	return &c, nil
}

func (c HealthClient) makeHandler(server string) handlers.Handler {
	return handlers.NewHealthHandler(server)
}

func (c HealthClient) makeCommands() (commands []string) {
	commands = append(commands, "health")
	return
}

func (c *HealthClient) Start(ctx context.Context, statsCh <-chan string) int {
	status := c.baseClient.Start(ctx, statsCh)

	switch status {
	case 0:
		if c.Serverless {
			fmt.Printf("WARNING: All seems fine but the check only run in serverless mode, please specify a remote server via --server hostname:port\n")
			return 1
		}
		fmt.Printf("OK: All fine at %s :-)\n", c.ServersStr)
	case 2:
		if c.Serverless {
			fmt.Printf("CRITICAL: DTail server not operating properly (using serverless connction)!\n")
			return 2
		}
		fmt.Printf("CRITICAL: DTail server not operating properly at %s!\n", c.ServersStr)
	default:
		if c.Serverless {
			fmt.Printf("UNKNOWN: Received unknown status code %d (using serverless connection)\n", status)
			return status
		}
		fmt.Printf("UNKNOWN: Received unknown status code %d from %s!\n", status, c.ServersStr)
	}

	return status
}
