package clients

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"slices"
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
	// endedByClient[i] reports whether the client itself ended the last
	// session of connections[i] (see runSession, recordSessionEnd and
	// snapshotSessions); replaceConnection resets it for the new connection.
	// recordSessionEnd grows it on demand, so it may be shorter than
	// connections: a missing entry reads as false, a session the client did
	// not end, or one not ended yet.
	endedByClient []bool
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
	// nowFn replaces time.Now in tests of the reconnect backoff.
	nowFn func() time.Time
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
			TrustChangedHostKeys:      c.TrustChangedHostKeys,
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

// ErrNoServers is the error of a client whose server discovery yielded no
// server to connect to, e.g. a --servers value of only whitespace and commas,
// an empty --servers value with a discovery module, a server list file of
// only blank and comment lines, or a discovery regex no server matches. Such
// a client would otherwise run no session at all and exit 0 with no output,
// and dtailhealth would report OK. It is exported so that the background
// jobs can tell this configuration problem from other creation failures.
var ErrNoServers = errors.New("no server to connect to")

// makeConnections discovers the servers and creates one connection per
// server. It fails with ErrNoServers when no server is left to connect to
// (see connectableServers). Scheduled and continuous jobs fail the same way
// when their client is created: the scheduler counts that as a failed run
// and backs the job off, writing no outfile, just as it does for a scheduled
// query that ran on no server (see scheduledQueryOutcome); a continuous job
// writes no outfile either and is tried again at its next retry, every
// minute, without a backoff.
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
	discovered, err := discoveryService.ServerList()
	if err != nil {
		return fmt.Errorf("discover servers: %w", err)
	}
	servers := connectableServers(discovered, c.Serverless)
	if len(servers) == 0 {
		return fmt.Errorf("discover servers: %w", ErrNoServers)
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

// connectableServers returns the discovered servers a client connects to.
// The entry "" names no server: it is the serverless sentinel the COMMA
// module returns for an empty server value (see
// discovery.ServerListFromCOMMA), and a serverless client opens its single
// session for it. A client that is not serverless (an empty --servers value
// with a discovery module, e.g. --discovery comma --servers "") drops it, so
// that it fails with ErrNoServers like a --servers value of only whitespace
// does, and not with an empty server address. The caller rejects an empty
// result whether or not the client is serverless: a serverless client always
// has one entry, the sentinel "" (an empty server value) or the word
// "serverless" (the COMMA module returns --servers serverless as it is),
// unless its server value names a server list file with no entry (a file
// named "serverless"), and then it would do nothing as well.
func connectableServers(servers []string, serverless bool) []string {
	if serverless {
		return servers
	}
	return slices.DeleteFunc(servers, func(server string) bool { return server == "" })
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
		// Collect unknown hosts into batches and trust them or ask the user about
		// them (see KnownHostsCallback.PromptAddHosts).
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
		sessionStart := c.now()
		status = c.runSession(ctx, i, conn)
		// A session that ran for a while was a healthy one: start the
		// backoff over. Otherwise the delay only ever grew, so that after
		// a handful of dserver restarts over the life of a long-running
		// dtail every further reconnect waited about a minute.
		if c.now().Sub(sessionStart) > maxRetryDelay {
			retryDelay = initialRetryDelay
		}

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

// runSession runs one session of conn, connection i, under a context of its
// own derived from ctx and returns the connection's exit status from its
// handler, raised for dcat and dgrep when the server reported a failed
// command or the session was cut short (see sessionStatus); the client exits
// with the highest one.
//
// Once the session ended, runSession decides whether the client ended it
// itself, i.e. ctx, the context of all of the client's connections, is done
// by then: SIGINT/SIGTERM cancel the context of Client.Start, and :quit and
// the end of the control TTY of an --interactive-query session cancel the
// context of runConnections, which ctx is then. Not the session's context:
// the connector cancels that itself when the session ends. The decision is
// made at the moment this session ended, so a session cut short (e.g. a
// server that went away) before a later cancel still counts as cut short.
// It is recorded for connection i by recordSessionEnd, and
// MaprClient.incompleteResultReasons reads it through snapshotSessions
// after all connections ended.
func (c *baseClient) runSession(ctx context.Context, i int, conn connectors.Connector) int {
	connCtx, cancel := context.WithCancel(ctx)
	conn.Start(connCtx, cancel, c.throttleCh, c.stats.connectionsEstCh)
	cancel()
	endedByClient := ctx.Err() != nil
	c.recordSessionEnd(i, endedByClient)
	return c.sessionStatus(endedByClient, conn.Handler())
}

// recordSessionEnd records whether the client ended the last session of
// connection i itself (see runSession).
func (c *baseClient) recordSessionEnd(i int, endedByClient bool) {
	mu := c.stateMu()
	mu.Lock()
	defer mu.Unlock()

	if i >= len(c.endedByClient) {
		c.endedByClient = append(c.endedByClient, make([]bool, i+1-len(c.endedByClient))...)
	}
	c.endedByClient[i] = endedByClient
}

// now returns the current time (see nowFn).
func (c *baseClient) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
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
		return connectors.NewServerless(args.UserName, c.profile.newHandler(server),
			commands, sessionSpec, args.InteractiveQuery, c.runtime, c.clientLogger()), nil
	}
	return connectors.NewServerConnection(server, args.UserName, sshAuthMethods,
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

// connectionSession is a connection together with whether the client ended
// its last session itself (see runSession).
type connectionSession struct {
	conn          connectors.Connector
	endedByClient bool
}

// snapshotSessions returns every connection with whether the client ended
// its last session itself, both read under one lock so that they stay in
// step with replaceConnection.
func (c *baseClient) snapshotSessions() []connectionSession {
	mu := c.stateMu()
	mu.RLock()
	defer mu.RUnlock()

	sessions := make([]connectionSession, len(c.connections))
	for i, conn := range c.connections {
		sessions[i].conn = conn
		sessions[i].endedByClient = i < len(c.endedByClient) && c.endedByClient[i]
	}
	return sessions
}

func (c *baseClient) storeReloadState(args config.Args, spec SessionSpec) {
	mu := c.stateMu()
	mu.Lock()
	defer mu.Unlock()

	c.Args = args
	c.sessionSpec = spec
}

// replaceConnection replaces connection i by conn, a new connection whose
// session has not ended yet, so the client did not end it either.
func (c *baseClient) replaceConnection(i int, conn connectors.Connector) {
	mu := c.stateMu()
	mu.Lock()
	defer mu.Unlock()

	c.connections[i] = conn
	if i < len(c.endedByClient) {
		c.endedByClient[i] = false
	}
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
