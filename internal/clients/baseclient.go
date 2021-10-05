package clients

import (
	"context"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/discovery"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
	"github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

// This is the main client data structure.
type baseClient struct {
	config.Args
	// To display client side stats
	stats *stats
	// List of remote servers to connect to.
	servers []string
	// We have one connection per remote server.
	connections []connectors.Connector
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
	// Regex is the regular expresion object for line filtering
	Regex regex.Regex
}

func (c *baseClient) init() {
	dlog.Client.Debug("Initiating base client")
	dlog.Client.Debug(c.Args.String())

	flag := regex.Default
	if c.Args.RegexInvert {
		flag = regex.Invert
	}
	regex, err := regex.New(c.Args.RegexStr, flag)
	if err != nil {
		dlog.Client.FatalPanic(c.Regex, "invalid regex!", err, regex)
	}
	c.Regex = regex
	dlog.Client.Debug("Regex", c.Regex)

	if c.Args.Serverless {
		return
	}
	c.sshAuthMethods, c.hostKeyCallback = client.InitSSHAuthMethods(c.Args.SSHAuthMethods, c.Args.SSHHostKeyCallback, c.Args.TrustAllHosts, c.throttleCh, c.Args.PrivateKeyPathFile)
}

func (c *baseClient) makeConnections(maker maker) {
	c.maker = maker

	discoveryService := discovery.New(c.Discovery, c.ServersStr, discovery.Shuffle)
	for _, server := range discoveryService.ServerList() {
		c.connections = append(c.connections, c.makeConnection(server, c.sshAuthMethods, c.hostKeyCallback))
	}

	c.stats = newTailStats(len(c.connections))
}

func (c *baseClient) Start(ctx context.Context, statsCh <-chan string) (status int) {
	// Can be nil when serverless.
	if c.hostKeyCallback != nil {
		// Periodically check for unknown hosts, and ask the user whether to trust them or not.
		go c.hostKeyCallback.PromptAddHosts(ctx)
	}
	// Print client stats every time something on statsCh is recieved.
	go c.stats.Start(ctx, c.throttleCh, statsCh, c.Args.Quiet)

	// Keep count of active connections
	active := make(chan struct{}, len(c.connections))

	var mutex sync.Mutex
	for i, conn := range c.connections {
		go func(i int, conn connectors.Connector) {
			connStatus := c.startConnection(ctx, active, i, conn)

			// Update global status.
			mutex.Lock()
			defer mutex.Unlock()
			if connStatus > status {
				status = connStatus
			}
		}(i, conn)
	}

	time.Sleep(time.Second * 2)
	c.waitUntilDone(ctx, active)
	return
}

func (c *baseClient) startConnection(ctx context.Context, active chan struct{}, i int, conn connectors.Connector) (status int) {
	// Increment connection count
	active <- struct{}{}
	// Derement connection count
	defer func() { <-active }()

	for {
		connCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		conn.Start(connCtx, cancel, c.throttleCh, c.stats.connectionsEstCh)
		// Retrieve status code from handler (dtail client will exit with that status)
		status = conn.Handler().Status()

		if !c.retry {
			return
		}

		time.Sleep(time.Second * 2)
		dlog.Client.Debug(conn.Server(), "Reconnecting")
		conn = c.makeConnection(conn.Server(), c.sshAuthMethods, c.hostKeyCallback)
		c.connections[i] = conn
	}
}

func (c *baseClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod, hostKeyCallback client.HostKeyCallback) connectors.Connector {
	if c.Args.Serverless {
		return connectors.NewServerless(c.UserName, c.maker.makeHandler(server), c.maker.makeCommands())
	}
	return connectors.NewServerConnection(server, c.UserName, sshAuthMethods,
		hostKeyCallback, c.maker.makeHandler(server), c.maker.makeCommands())
}

func (c *baseClient) waitUntilDone(ctx context.Context, active chan struct{}) {
	defer dlog.Client.Debug("Terminated connection")

	// We want to have at least one active connection
	<-active
	// Put it back on the channel
	active <- struct{}{}

	if c.Mode == omode.TailClient && c.retry {
		<-ctx.Done()
	}

	// TODO: Rewrite this to use a wait group.
	for {
		numActive := len(active)
		if numActive == 0 {
			return
		}
		dlog.Client.Debug("Active connections", numActive)
		time.Sleep(time.Second * time.Millisecond * 100)
	}
}
