package clients

import (
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
	args.UserName = config.ControlUser
	c := HealthClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
	}

	c.init()
	c.sshAuthMethods = append(c.sshAuthMethods, gossh.Password(config.ControlUser))
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
