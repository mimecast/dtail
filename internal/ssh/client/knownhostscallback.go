package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/prompt"
	"github.com/mimecast/dtail/internal/logging"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const unknownHostsPromptDelay = 2 * time.Second

type response int

const (
	trustHost     response = iota
	dontTrustHost response = iota
)

// Represents an unknown host.
type unknownHost struct {
	server     string
	remote     net.Addr
	key        ssh.PublicKey
	hostLine   string
	ipLine     string
	responseCh chan response
}

// KnownHostsCallback is a wrapper around ssh.KnownHosts so that we can add all
// unknown hosts in a single batch to the known_hosts file.
type KnownHostsCallback struct {
	knownHostsPath  string
	knownHostsFile  fs.RootedPath
	unknownCh       chan unknownHost
	trustAllHostsCh chan struct{}
	// trustAllOnce guards the single close of trustAllHostsCh. The old
	// select/default/close pattern was not atomic: two concurrent callers
	// could both observe the channel open, both fall through to the default
	// branch, and both call close() — causing a panic. sync.Once makes the
	// close idempotent and race-free without any additional locking.
	trustAllOnce   sync.Once
	untrustedHosts map[string]bool
	mutex          *sync.Mutex
	logger         logging.Logger
	promptLogger   logging.Logger
	removeTempFile func(*os.Root, string) error
	// lockTimeout bounds the wait for another client's known_hosts update.
	lockTimeout time.Duration
}

var _ HostKeyCallback = (*KnownHostsCallback)(nil)

// NewKnownHostsCallback returns a new wrapper.
func NewKnownHostsCallback(knownHostsPath string, trustAllHosts bool,
	logger, promptLogger logging.Logger) (HostKeyCallback, error) {

	knownHostsFile, err := fs.NewRootedPath(knownHostsPath)
	if err != nil {
		return nil, err
	}
	ensureKnownHostsFile(knownHostsFile)
	untrustedHosts := make(map[string]bool)

	c := KnownHostsCallback{
		knownHostsPath:  knownHostsPath,
		knownHostsFile:  knownHostsFile,
		unknownCh:       make(chan unknownHost),
		trustAllHostsCh: make(chan struct{}),
		untrustedHosts:  untrustedHosts,
		mutex:           &sync.Mutex{},
		logger:          logging.OrNop(logger),
		promptLogger:    logging.OrNop(promptLogger),
		removeTempFile:  func(root *os.Root, name string) error { return root.Remove(name) },
		lockTimeout:     knownHostsLockTimeout,
	}
	if trustAllHosts {
		// Use the same sync.Once path so both the constructor and the
		// interactive "all" prompt are idempotent and race-free.
		c.closeTrustAllHostsCh()
	}
	return &c, nil
}

// closeTrustAllHostsCh closes trustAllHostsCh exactly once via sync.Once,
// regardless of how many goroutines call it concurrently. This replaces the
// former select/default/close pattern which was not atomic: two concurrent
// callers could both observe the channel open, both take the default branch,
// and both call close() — causing a panic.
func (c *KnownHostsCallback) closeTrustAllHostsCh() {
	c.trustAllOnce.Do(func() { close(c.trustAllHostsCh) })
}

func ensureKnownHostsFile(knownHostsFile fs.RootedPath) {
	root, err := knownHostsFile.OpenRoot()
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()

	fd, err := root.OpenFile(knownHostsFile.Name(), os.O_RDONLY|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	_ = fd.Close()
}

// Wrap the host key callback. The returned ssh.HostKeyCallback is bound to
// ctx: if ctx is cancelled while we are waiting for the PromptAddHosts
// goroutine to consume an unknown host or to return a user decision, the
// callback aborts with ctx.Err() instead of blocking forever. This prevents
// a stuck SSH handshake (and a leaked goroutine per unknown host) when the
// client shuts down before the user responds, or when PromptAddHosts has
// already returned because its ctx was cancelled.
func (c *KnownHostsCallback) Wrap(ctx context.Context) ssh.HostKeyCallback {
	return func(server string, remote net.Addr, key ssh.PublicKey) error {
		// Parse known_hosts file
		knownHostsCb, err := knownhosts.New(c.knownHostsPath)
		if err != nil {
			return err
		}
		// Check for valid entry in known_hosts file
		err = knownHostsCb(server, remote, key)
		if err == nil {
			// OK
			return nil
		}

		unknown := unknownHost{
			server:     server,
			remote:     remote,
			key:        key,
			hostLine:   knownhosts.Line([]string{server}, key),
			ipLine:     knownhosts.Line([]string{remote.String()}, key),
			responseCh: make(chan response, 1),
		}
		// Keep host trust discovery diagnostics out of normal command output.
		// In trust-all and plain modes this warning can corrupt tool output.
		c.logger.Debug("Encountered unknown host", unknown.server, unknown.remote.String())
		// Notify user that there is an unknown host. Honour ctx cancellation
		// so we do not block forever when PromptAddHosts has already exited.
		select {
		case c.unknownCh <- unknown:
		case <-ctx.Done():
			return fmt.Errorf("host key callback cancelled for %s: %w", server, ctx.Err())
		}
		// Wait for user input. Same contract as above: abort on ctx cancel.
		var resp response
		select {
		case resp = <-unknown.responseCh:
		case <-ctx.Done():
			return fmt.Errorf("host key callback cancelled for %s: %w", server, ctx.Err())
		}
		switch resp {
		case trustHost:
			// End user acknowledged host key
			return nil
		case dontTrustHost:
		}

		c.mutex.Lock()
		defer c.mutex.Unlock()
		c.untrustedHosts[server] = true
		return err
	}
}

// PromptAddHosts prompts a question to the user whether unknown hosts should
// be added to the known hosts or not.
func (c *KnownHostsCallback) PromptAddHosts(ctx context.Context) {
	var hosts []unknownHost
	timer := time.NewTimer(unknownHostsPromptDelay)
	defer timer.Stop()

	for {
		// Check whether there is a unknown host
		select {
		case unknown := <-c.unknownCh:
			hosts = append(hosts, unknown)
			// Ask every 50 unknown hosts
			if len(hosts) >= 50 {
				c.promptAddHosts(hosts)
				hosts = []unknownHost{}
			}
		case <-timer.C:
			// Or ask when after 2 seconds no new unknown hosts were added.
			if len(hosts) > 0 {
				c.promptAddHosts(hosts)
				hosts = []unknownHost{}
			}
		case <-ctx.Done():
			c.logger.Debug("Stopping goroutine prompting new hosts...")
			return
		}
		restartTimer(timer, unknownHostsPromptDelay)
	}
}

func restartTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func (c *KnownHostsCallback) promptAddHosts(hosts []unknownHost) {
	var servers []string
	for _, host := range hosts {
		servers = append(servers, host.server)
	}

	select {
	case <-c.trustAllHostsCh:
		// Trust-all mode is non-interactive; avoid warning-level noise on stdout.
		c.logger.Debug("Trusting host keys of servers", servers)
		unlocked, err := c.trustHosts(hosts)
		if err != nil {
			c.logger.Error("Unable to update known hosts file", c.knownHostsPath, err)
			c.dontTrustHosts(hosts)
			return
		}
		c.logUnlockedUpdate(unlocked)
		return
	default:
	}

	question := fmt.Sprintf("Encountered %d unknown hosts: '%s'\n%s",
		len(servers),
		strings.Join(servers, ","),
		"Do you want to trust these hosts?",
	)
	p := prompt.New(question, c.promptLogger)

	// The prompt pauses logging while it runs the callbacks, so their
	// results are logged from the end callbacks once logging has resumed.
	var trustUnlocked, trustErr error
	a := prompt.Answer{
		Long:  "yes",
		Short: "y",
		Callback: func() {
			trustUnlocked, trustErr = c.trustHosts(hosts)
			if trustErr != nil {
				c.dontTrustHosts(hosts)
			}
		},
		EndCallback: func() {
			c.logTrustHostsResult(trustUnlocked, trustErr)
		},
	}
	p.Add(a)

	var trustAllUnlocked, trustAllErr error
	a = prompt.Answer{
		Long:  "all",
		Short: "a",
		Callback: func() {
			trustAllUnlocked, trustAllErr = c.trustHosts(hosts)
			if trustAllErr != nil {
				c.dontTrustHosts(hosts)
				return
			}
			// Mark trust-all atomically so that concurrent "all" callbacks
			// from other batches do not double-close the channel.
			c.closeTrustAllHostsCh()
		},
		EndCallback: func() {
			c.logTrustHostsResult(trustAllUnlocked, trustAllErr)
		},
	}
	p.Add(a)

	a = prompt.Answer{
		Long:  "no",
		Short: "n",
		Callback: func() {
			c.dontTrustHosts(hosts)
		},
		EndCallback: func() {
			c.logger.Info("Didn't add hosts to known hosts file", c.knownHostsPath)
		},
	}
	p.Add(a)

	a = prompt.Answer{
		Long:     "details",
		Short:    "d",
		AskAgain: true,
		Callback: func() {
			for _, unknown := range hosts {
				fmt.Println(unknown.hostLine)
				fmt.Println(unknown.ipLine)
			}
		},
	}
	p.Add(a)

	p.Ask(os.Stdin)
}

func (c *KnownHostsCallback) logTrustHostsResult(unlocked, err error) {
	if err != nil {
		c.logger.Error("Unable to update known hosts file", c.knownHostsPath, err)
		return
	}
	c.logUnlockedUpdate(unlocked)
	c.logger.Info("Added hosts to known hosts file", c.knownHostsPath)
}

// logUnlockedUpdate reports a successful known_hosts update that ran without
// the lock, which may have dropped another client's concurrent additions.
// Platforms without advisory locking never lock, so that is only a debug note.
func (c *KnownHostsCallback) logUnlockedUpdate(unlocked error) {
	switch {
	case unlocked == nil:
	case errors.Is(unlocked, errors.ErrUnsupported):
		c.logger.Debug("Updated known hosts file without a lock", c.knownHostsPath, unlocked)
	default:
		c.logger.Warn("Updated known hosts file without a lock; concurrent "+
			"updates by other clients may have been lost", c.knownHostsPath, unlocked)
	}
}

// trustHosts adds hosts to known_hosts and then trusts them. A nil err with a
// non-nil unlocked means the update succeeded without the lock, for the reason
// in unlocked; if err is non-nil it already includes that reason.
func (c *KnownHostsCallback) trustHosts(hosts []unknownHost) (unlocked, err error) {
	unlocked, err = c.updateKnownHosts(hosts)
	if err != nil {
		return unlocked, err
	}
	for _, unknown := range hosts {
		unknown.responseCh <- trustHost
	}
	return unlocked, nil
}

// updateKnownHosts rewrites known_hosts with hosts first, followed by every
// existing entry whose address none of hosts replaces. Several dtail clients
// may do this at the same time, so the read-merge-write runs under an
// advisory lock and the new content goes through a uniquely named temporary
// file that is renamed over known_hosts atomically. Without the lock (see
// lockKnownHosts) the update still succeeds, but may drop another client's
// concurrent additions.
func (c *KnownHostsCallback) updateKnownHosts(hosts []unknownHost) (unlocked, err error) {
	root, rootErr := c.knownHostsFile.OpenRoot()
	if rootErr != nil {
		return nil, rootErr
	}
	defer func() { _ = root.Close() }()

	release, lockErr := lockKnownHosts(root, c.knownHostsFile.Name(), c.lockTimeout)
	defer release()

	if err := c.replaceKnownHosts(root, hosts); err != nil {
		if lockErr != nil {
			return lockErr, errors.Join(err, lockErr)
		}
		return nil, err
	}
	return lockErr, nil
}

func (c *KnownHostsCallback) replaceKnownHosts(root *os.Root, hosts []unknownHost) error {
	newFd, tmpKnownHostsName, createErr := createKnownHostsTemp(root, c.knownHostsFile.Name())
	if createErr != nil {
		return fmt.Errorf("create temp known hosts file for %s: %w", c.knownHostsPath, createErr)
	}
	tmpKnownHostsPath := filepath.Join(filepath.Dir(c.knownHostsPath), tmpKnownHostsName)
	cleanupTmp := func() error {
		if removeErr := c.removeTempFile(root, tmpKnownHostsName); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("remove temporary known hosts file %s: %w", tmpKnownHostsPath, removeErr)
		}
		return nil
	}

	if err := c.writeKnownHosts(root, newFd, tmpKnownHostsPath, hosts); err != nil {
		return errors.Join(err, cleanupTmp())
	}
	// Now, replace old known hosts file
	if err := root.Rename(tmpKnownHostsName, c.knownHostsFile.Name()); err != nil {
		return errors.Join(fmt.Errorf("replace known_hosts file %s: %w", c.knownHostsPath, err), cleanupTmp())
	}
	return nil
}

// writeKnownHosts fills the temporary file newFd and always closes it.
func (c *KnownHostsCallback) writeKnownHosts(root *os.Root, newFd *os.File,
	tmpKnownHostsPath string, hosts []unknownHost) error {

	if chmodErr := newFd.Chmod(0o600); chmodErr != nil {
		return errors.Join(fmt.Errorf("chmod temp known hosts file %s: %w", tmpKnownHostsPath, chmodErr),
			closeKnownHostsFile(newFd, tmpKnownHostsPath))
	}
	if err := c.mergeKnownHosts(root, newFd, hosts); err != nil {
		return errors.Join(err, closeKnownHostsFile(newFd, tmpKnownHostsPath))
	}
	if err := newFd.Close(); err != nil {
		return fmt.Errorf("close temp known hosts file %s: %w", tmpKnownHostsPath, err)
	}
	return nil
}

// mergeKnownHosts writes the lines of hosts, then every line of the current
// known_hosts file whose address is not one of the newly trusted ones.
func (c *KnownHostsCallback) mergeKnownHosts(root *os.Root, newFd io.Writer, hosts []unknownHost) error {
	// Newly trusted hosts in normalized form
	addresses := make(map[string]struct{})
	// First write to new known hosts file, and keep track of addresses
	for _, unknown := range hosts {
		// Add once as [HOSTNAME]:PORT
		addresses[knownhosts.Normalize(unknown.server)] = struct{}{}
		// And once as [IP]:PORT
		addresses[knownhosts.Normalize(unknown.remote.String())] = struct{}{}

		if _, writeErr := fmt.Fprintf(newFd, "%s\n", unknown.hostLine); writeErr != nil {
			return fmt.Errorf("write host known_hosts entry: %w", writeErr)
		}
		if _, writeErr := fmt.Fprintf(newFd, "%s\n", unknown.ipLine); writeErr != nil {
			return fmt.Errorf("write ip known_hosts entry: %w", writeErr)
		}
	}

	// Read old known hosts file, to see which are old and new entries
	oldFd, oldOpenErr := root.OpenFile(c.knownHostsFile.Name(), os.O_RDONLY|os.O_CREATE, 0o600)
	if oldOpenErr != nil {
		return fmt.Errorf("open known hosts file %s: %w", c.knownHostsPath, oldOpenErr)
	}

	scanner := bufio.NewScanner(oldFd)
	// Now, append all still valid old entries to the new host file
	for scanner.Scan() {
		line := scanner.Text()
		address := strings.SplitN(line, " ", 2)[0]

		if _, ok := addresses[address]; !ok {
			if _, writeErr := fmt.Fprintf(newFd, "%s\n", line); writeErr != nil {
				return errors.Join(fmt.Errorf("append existing known_hosts entry: %w", writeErr),
					closeKnownHostsFile(oldFd, c.knownHostsPath))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.Join(fmt.Errorf("scan existing known_hosts entries: %w", err),
			closeKnownHostsFile(oldFd, c.knownHostsPath))
	}
	return closeKnownHostsFile(oldFd, c.knownHostsPath)
}

func closeKnownHostsFile(fd *os.File, path string) error {
	if err := fd.Close(); err != nil {
		return fmt.Errorf("close known hosts file %s: %w", path, err)
	}
	return nil
}

func (c *KnownHostsCallback) dontTrustHosts(hosts []unknownHost) {
	for _, unknown := range hosts {
		unknown.responseCh <- dontTrustHost
	}
}

// Untrusted returns true if the host is not trusted. False otherwise.
func (c *KnownHostsCallback) Untrusted(server string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	_, ok := c.untrustedHosts[server]
	return ok
}
