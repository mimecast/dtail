package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/discovery"
	"github.com/mimecast/dtail/internal/regex"
	"github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

const (
	initialRetryDelay = 2 * time.Second
	maxRetryDelay     = 60 * time.Second
	retryJitterFactor = 0.2 // +/-20% jitter to avoid synchronized reconnect storms.
)

// This is the main client data structure.
type baseClient struct {
	mu *sync.RWMutex
	config.Args
	cfg       config.RuntimeConfig
	runtime   *clientRuntimeBoundary
	colorizer *brush.Brush
	loggers   LoggerDependencies
	// To display client side stats
	stats *stats
	// We have one connection per remote server.
	connections []connectors.Connector
	// SSH auth methods to use to connect to the remote servers.
	sshAuthMethods []gossh.AuthMethod
	// authCloser owns any ssh-agent connection acquired while building the
	// auth methods; it must be closed once all SSH handshakes that consume
	// sshAuthMethods have completed.
	authCloser io.Closer
	// To deal with SSH host keys
	hostKeyCallback client.HostKeyCallback
	// Throttle how fast we initiate SSH connections concurrently
	throttleCh chan struct{}
	// The current connection-wide session specification.
	sessionSpec SessionSpec
	// Client-specific connection behavior.
	profile clientProfile
	// Optional factory override for retry/reconnect tests.
	connectionFactory func(server string, authMethods []gossh.AuthMethod,
		hostKeyCallback client.HostKeyCallback, sessionSpec SessionSpec,
		interactive bool) connectors.Connector
	// Optional sleep override for retry tests.
	sleepFn func(context.Context, time.Duration) bool
	// Regex is the regular expresion object for line filtering
	Regex regex.Regex
}

func newBaseClient(args config.Args, cfg config.RuntimeConfig, loggers LoggerDependencies,
	colorizer *brush.Brush, profile clientProfile) (baseClient, error) {
	c := baseClient{
		mu:          newBaseClientMu(),
		Args:        args,
		cfg:         cfg,
		colorizer:   colorizer,
		loggers:     loggers.normalized(),
		throttleCh:  make(chan struct{}, args.ConnectionsPerCPU*runtime.GOMAXPROCS(0)),
		sessionSpec: NewSessionSpec(args),
		profile:     profile,
	}
	if err := c.initialize(); err != nil {
		return baseClient{}, err
	}
	return c, nil
}

func (c *baseClient) clientLogger() clientlog.Logger {
	if c.loggers.Client == nil {
		return clientlog.NopLogger{}
	}
	return c.loggers.Client
}

func firstColorizer(colorizers []*brush.Brush) *brush.Brush {
	if len(colorizers) == 0 {
		return nil
	}
	return colorizers[0]
}

func (c *baseClient) init() error {
	c.loggers = c.loggers.normalized()
	c.clientLogger().Debug("Initiating base client", c.String())
	if c.runtime == nil {
		c.runtime = newClientRuntimeBoundary(c.cfg, c.loggers, c.colorizer)
	}

	flag := regex.Default
	if c.RegexInvert {
		flag = regex.Invert
	}
	compiledRegex, err := regex.New(c.RegexStr, flag)
	if err != nil {
		return fmt.Errorf("compile regular expression %q: %w", c.RegexStr, err)
	}
	c.Regex = compiledRegex

	if c.Serverless {
		return nil
	}
	sshAuthMethods, hostKeyCallback, authCloser, err := client.InitSSHAuthMethods(
		c.SSHAuthMethods, c.SSHHostKeyCallback, client.AuthMethodConfig{
			TrustAllHosts:             c.TrustAllHosts,
			KnownHostsPath:            c.KnownHostsPath,
			PrivateKeyPath:            c.SSHPrivateKeyFilePath,
			AdditionalPrivateKeyPaths: c.SSHPrivateKeyFallbackPaths,
			AgentKeyIndex:             c.SSHAgentKeyIndex,
		}, c.clientLogger(), c.loggers.Common)
	if err != nil {
		return fmt.Errorf("initialize SSH authentication: %w", err)
	}
	c.sshAuthMethods = sshAuthMethods
	c.hostKeyCallback = hostKeyCallback
	c.authCloser = authCloser
	return nil
}

func (c *baseClient) initialize() error {
	if err := c.init(); err != nil {
		return err
	}
	if err := c.makeConnections(); err != nil {
		if closeErr := c.closeAuth(); closeErr != nil {
			return errors.Join(err, fmt.Errorf("close SSH authentication resources: %w", closeErr))
		}
		return err
	}
	return nil
}

func (c *baseClient) closeAuth() error {
	if c.authCloser == nil {
		return nil
	}
	closer := c.authCloser
	c.authCloser = nil
	return closer.Close()
}

func (c *baseClient) makeConnections() error {
	if c.profile.newHandler == nil {
		return errors.New("client handler factory is required")
	}
	if _, err := c.sessionSpec.Commands(); err != nil {
		return fmt.Errorf("build session commands: %w", err)
	}

	discoveryService, err := discovery.New(c.Discovery, c.ServersStr, discovery.Shuffle, c.clientLogger())
	if err != nil {
		return fmt.Errorf("configure server discovery: %w", err)
	}
	servers, err := discoveryService.ServerList()
	if err != nil {
		return fmt.Errorf("discover servers: %w", err)
	}
	for _, server := range servers {
		connection, connectionErr := c.makeConnection(server, c.sshAuthMethods, c.hostKeyCallback)
		if connectionErr != nil {
			return fmt.Errorf("create connection for %q: %w", server, connectionErr)
		}
		c.connections = append(c.connections, connection)
	}

	c.stats = newTailStats(len(c.connections), c.runtime.output, c.runtime.InterruptPause(), c.clientLogger())
	return nil
}

func (c *baseClient) Start(ctx context.Context, statsCh <-chan string) (status int) {
	if c.InteractiveQuery {
		return c.startInteractiveControl(ctx, statsCh)
	}
	return c.runConnections(ctx, statsCh)
}

func (c *baseClient) runConnections(ctx context.Context, statsCh <-chan string) (status int) {
	c.clientLogger().Trace("Starting base client")
	// Release the ssh-agent connection (if any) once all handshakes and
	// reconnect attempts that consume c.sshAuthMethods have finished.
	if c.authCloser != nil {
		defer func() {
			if err := c.closeAuth(); err != nil {
				c.clientLogger().Debug("baseClient", "failed to close ssh-agent connection", err)
			}
		}()
	}
	// Can be nil when serverless.
	if c.hostKeyCallback != nil {
		// Periodically check for unknown hosts, and ask the user whether to trust them or not.
		go c.hostKeyCallback.PromptAddHosts(ctx)
	}
	// Print client stats every time something on statsCh is received.
	go c.stats.Start(ctx, c.throttleCh, statsCh, c.Quiet)

	var wg sync.WaitGroup
	connections := c.snapshotConnections()
	wg.Add(len(connections))
	var mutex sync.Mutex

	for i, conn := range connections {
		go func(i int, conn connectors.Connector) {
			defer wg.Done()
			connStatus := c.startConnection(ctx, i, conn)
			mutex.Lock()
			defer mutex.Unlock()
			if connStatus > status {
				status = connStatus
			}
		}(i, conn)
	}

	wg.Wait()
	return
}

func (c *baseClient) startConnection(ctx context.Context, i int,
	conn connectors.Connector) (status int) {

	retryDelay := initialRetryDelay
	retryRandom := newRetryRandom(i)

	for {
		connCtx, cancel := context.WithCancel(ctx)

		conn.Start(connCtx, cancel, c.throttleCh, c.stats.connectionsEstCh)
		cancel()
		// Retrieve status code from handler (dtail client will exit with that status)
		status = conn.Handler().Status()

		// Do we want to retry?
		if !c.profile.retry {
			// No, we don't.
			return
		}
		select {
		case <-ctx.Done():
			// No, context is done, so no retry.
			return
		default:
		}

		// Yes, we want to retry with exponential backoff and jitter.
		sleepDuration := jitterRetryDelay(retryDelay, retryRandom)
		c.clientLogger().Debug(conn.Server(), "Reconnecting", "backoff", sleepDuration)
		if !c.sleepRetry(ctx, sleepDuration) {
			return
		}

		retryDelay = nextRetryDelay(retryDelay)
		server := conn.Server()
		var err error
		conn, err = c.makeConnection(server, c.sshAuthMethods, c.hostKeyCallback)
		if err != nil {
			c.clientLogger().Error(server, "Unable to recreate connection", err)
			if status == 0 {
				status = 1
			}
			return
		}
		c.replaceConnection(i, conn)
	}
}

func nextRetryDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return initialRetryDelay
	}

	next := current * 2
	if next > maxRetryDelay || next < current {
		return maxRetryDelay
	}
	return next
}

func jitterRetryDelay(base time.Duration, random *rand.Rand) time.Duration {
	if base <= 0 || random == nil {
		return base
	}

	jitter := time.Duration(float64(base) * retryJitterFactor)
	if jitter <= 0 {
		return base
	}

	minDelay := base - jitter
	maxDelay := base + jitter
	if maxDelay < minDelay {
		return base
	}

	return minDelay + time.Duration(random.Int63n(int64(maxDelay-minDelay+1)))
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newRetryRandom(seedOffset int) *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano() + int64(seedOffset)))
}

func (c *baseClient) makeConnection(server string, sshAuthMethods []gossh.AuthMethod,
	hostKeyCallback client.HostKeyCallback) (connectors.Connector, error) {
	args, sessionSpec := c.snapshotConnectionState()
	return c.makeConnectionWithState(server, sshAuthMethods, hostKeyCallback, args, sessionSpec)
}

func (c *baseClient) makeConnectionWithState(server string, sshAuthMethods []gossh.AuthMethod,
	hostKeyCallback client.HostKeyCallback, args config.Args, sessionSpec SessionSpec) (connectors.Connector, error) {
	if c.connectionFactory != nil {
		return c.connectionFactory(server, sshAuthMethods, hostKeyCallback,
			sessionSpec, args.InteractiveQuery), nil
	}
	commands, err := sessionSpec.Commands()
	if err != nil {
		return nil, fmt.Errorf("build commands for %q: %w", server, err)
	}
	if args.Serverless {
		return connectors.NewServerless(c.UserName, c.profile.newHandler(server),
			commands, sessionSpec, args.InteractiveQuery, c.runtime, c.clientLogger()), nil
	}
	return connectors.NewServerConnection(server, c.UserName, sshAuthMethods,
		hostKeyCallback, c.profile.newHandler(server), commands,
		sessionSpec, args.InteractiveQuery, args.SSHPrivateKeyFilePath,
		args.NoAuthKey, c.runtime, c.clientLogger())
}

func (c *baseClient) sleepRetry(ctx context.Context, delay time.Duration) bool {
	if c.sleepFn != nil {
		return c.sleepFn(ctx, delay)
	}
	return sleepWithContext(ctx, delay)
}

func (c *baseClient) snapshotConnectionState() (config.Args, SessionSpec) {
	mu := c.stateMu()
	mu.RLock()
	defer mu.RUnlock()

	return c.Args, c.sessionSpec
}

func (c *baseClient) snapshotMutableState() (config.Args, SessionSpec, []connectors.Connector) {
	mu := c.stateMu()
	mu.RLock()
	defer mu.RUnlock()

	return c.Args, c.sessionSpec, append([]connectors.Connector(nil), c.connections...)
}

func (c *baseClient) snapshotConnections() []connectors.Connector {
	mu := c.stateMu()
	mu.RLock()
	defer mu.RUnlock()

	return append([]connectors.Connector(nil), c.connections...)
}

func (c *baseClient) storeReloadState(args config.Args, spec SessionSpec) {
	mu := c.stateMu()
	mu.Lock()
	defer mu.Unlock()

	c.Args = args
	c.sessionSpec = spec
}

func (c *baseClient) replaceConnection(i int, conn connectors.Connector) {
	mu := c.stateMu()
	mu.Lock()
	defer mu.Unlock()

	c.connections[i] = conn
}

func (c *baseClient) stateMu() *sync.RWMutex {
	if c.mu == nil {
		c.mu = newBaseClientMu()
	}
	return c.mu
}

func newBaseClientMu() *sync.RWMutex {
	return &sync.RWMutex{}
}
