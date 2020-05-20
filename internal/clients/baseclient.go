package clients

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/remote"
	"github.com/mimecast/dtail/internal/discovery"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

// This is the main client data structure.
type baseClient struct {
	Args
	// To display client side stats
	stats *stats
	// List of remote servers to connect to.
	servers []string
	// We have one connection per remote server.
	connections []*remote.Connection
	// SSH auth methods to use to connect to the remote servers.
	sshAuthMethods []gossh.AuthMethod
	// To deal with SSH host keys
	hostKeyCallback client.HostKeyCallback
	// Throttle how fast we initiate SSH connections concurrently
	throttleCh chan struct{}
	// Retry connection upon failure?
	retry bool
	// Connection maker helper.
	maker maker
}

func (c *baseClient) init(maker maker) {
	logger.Info("Initiating base client")

	c.maker = maker
	c.sshAuthMethods, c.hostKeyCallback = client.InitSSHAuthMethods(c.Args.SSHAuthMethods, c.Args.SSHHostKeyCallback, c.Args.TrustAllHosts, c.throttleCh, c.Args.PrivateKeyPathFile)

	discoveryService := discovery.New(c.Discovery, c.ServersStr, discovery.Shuffle)

	for _, server := range discoveryService.ServerList() {
		c.connections = append(c.connections, c.makeConnection(server, c.sshAuthMethods, c.hostKeyCallback))
	}

	if _, err := regexp.Compile(c.Regex); err != nil {
		logger.FatalExit(c.Regex, "Can't test compile regex", err)
	}

	c.stats = newTailStats(len(c.connections))
}

func (c *baseClient) Start(ctx context.Context) (status int) {
	// Periodically check for unknown hosts, and ask the user whether to trust them or not.
	go c.hostKeyCallback.PromptAddHosts(ctx)
	// Periodically print out connection stats to the client.
	go c.stats.periodicLogStats(ctx, c.throttleCh)
	// Keep count of active connections
	active := make(chan struct{}, len(c.connections))

	var mutex sync.Mutex
	for i, conn := range c.connections {
		go func(i int, conn *remote.Connection) {
			connStatus := c.start(ctx, active, i, conn)

			// Update global status.
			mutex.Lock()
			defer mutex.Unlock()
			if connStatus > status {
				status = connStatus
			}
		}(i, conn)
	}

	c.waitUntilDone(ctx, active)
	return
}

func (c *baseClient) start(ctx context.Context, active chan struct{}, i int, conn *remote.Connection) (status int) {
	// Increment connection count
	active <- struct{}{}
	// Derement connection count
	defer func() { <-active }()

	for {
		connCtx, cancel := conn.Handler.WithCancel(ctx)
		defer cancel()

		conn.Start(connCtx, cancel, c.throttleCh, c.stats.connectionsEstCh)
		// Retrieve status code from handler (dtail client will exit with that status)
		status = conn.Handler.Status()

		if !c.retry {
			return
		}

		time.Sleep(time.Second * 2)
		logger.Debug(conn.Server, "Reconnecting")

		conn = c.makeConnection(conn.Server, c.sshAuthMethods, c.hostKeyCallback)
		c.connections[i] = conn
	}
}

func (c *baseClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback client.HostKeyCallback) *remote.Connection {
	conn := remote.NewConnection(server, c.UserName, sshAuthMethods, hostKeyCallback)
	conn.Handler = c.maker.makeHandler(server)
	conn.Commands = c.maker.makeCommands()

	return conn
}

func (c *baseClient) waitUntilDone(ctx context.Context, active chan struct{}) {
	defer logger.Info("Terminated connection")

	// We want to have at least one active connection
	<-active
	// Put it back on the channel
	active <- struct{}{}

	if c.Mode == omode.TailClient && c.retry {
		<-ctx.Done()
	}

	for {
		numActive := len(active)
		if numActive == 0 {
			return
		}
		logger.Debug("Active connections", numActive)
		time.Sleep(time.Second)
	}
}
