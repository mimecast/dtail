package clients

import (
	"regexp"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/remote"
	"github.com/mimecast/dtail/internal/discovery"
	"github.com/mimecast/dtail/internal/logger"
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
	hostKeyCallback *client.HostKeyCallback
	// To stop the client.
	stop chan struct{}
	// To indicate that the client has stopped.
	stopped chan struct{}
	// Throttle how fast we initiate SSH connections concurrently
	throttleCh chan struct{}
	// Retry connection upon failure?
	retry bool
	// Connection helper.
	maker connectionMaker
}

func (c *baseClient) init(maker connectionMaker) {
	logger.Info("Initiating base client")

	c.maker = maker
	//c.connections = make(map[string]*remote.Connection)
	c.sshAuthMethods, c.hostKeyCallback = client.InitSSHAuthMethods(c.TrustAllHosts, c.throttleCh)

	// Retrieve a shuffled list of remote dtail servers.
	shuffleServers := true
	discoveryService := discovery.New(c.Discovery, c.ServersStr, shuffleServers)
	for _, server := range discoveryService.ServerList() {
		c.connections = append(c.connections, c.maker.makeConnection(server, c.sshAuthMethods, c.hostKeyCallback))
	}

	if _, err := regexp.Compile(c.Regex); err != nil {
		logger.FatalExit(c.Regex, "Can't test compile regex", err)
	}

	// Periodically check for unknown hosts, and ask the user whether to trust them or not.
	go c.hostKeyCallback.PromptAddHosts(c.stop)

	// Periodically print out connection stats to the client.
	c.stats = newTailStats(len(c.connections))
	go c.stats.periodicLogStats(c.throttleCh, c.stop)
}

func (c *baseClient) Start() (status int) {
	active := make(chan struct{}, len(c.connections))

	var wg sync.WaitGroup
	wg.Add(len(c.connections))

	for i, conn := range c.connections {
		go func(i int, conn *remote.Connection) {
			active <- struct{}{}
			defer func() {
				logger.Debug(conn.Server, "Disconnected completely...")
				<-active
			}()
			wg.Done()

			for {
				conn.Start(c.throttleCh, c.stats.connectionsEstCh)
				if !c.retry {
					return
				}
				time.Sleep(time.Second * 2)
				logger.Debug(conn.Server, "Reconencting")
				conn = c.maker.makeConnection(conn.Server, c.sshAuthMethods, c.hostKeyCallback)
				c.connections[i] = conn
			}
		}(i, conn)
	}

	wg.Wait()
	c.waitUntilDone(active)

	return
}

func (c *baseClient) waitUntilDone(active chan struct{}) {
	defer close(c.stopped)

	if c.Mode != omode.TailClient {
		c.waitUntilZero(active)
		logger.Info("All connections stopped")
		return
	}

	<-c.stop
	logger.Info("Stopping client")
	for _, conn := range c.connections {
		conn.Stop()
	}

	c.waitUntilZero(active)
}

func (c *baseClient) waitUntilZero(active chan struct{}) {
	for {
		logger.Debug("Active connections", len(active))
		if len(active) == 0 {
			return
		}
		time.Sleep(time.Second)
	}
}

func (c *baseClient) Stop() {
	close(c.stop)
	<-c.WaitC()
}

func (c *baseClient) WaitC() <-chan struct{} {
	return c.stopped
}
