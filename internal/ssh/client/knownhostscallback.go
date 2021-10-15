package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/prompt"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

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
	unknownCh       chan unknownHost
	throttleCh      chan struct{}
	trustAllHostsCh chan struct{}
	untrustedHosts  map[string]bool
	mutex           *sync.Mutex
}

// NewKnownHostsCallback returns a new wrapper.
func NewKnownHostsCallback(knownHostsPath string, trustAllHosts bool,
	throttleCh chan struct{}) (HostKeyCallback, error) {

	os.OpenFile(knownHostsPath, os.O_RDONLY|os.O_CREATE, 0666)
	untrustedHosts := make(map[string]bool)

	c := KnownHostsCallback{
		knownHostsPath:  knownHostsPath,
		unknownCh:       make(chan unknownHost),
		trustAllHostsCh: make(chan struct{}),
		throttleCh:      throttleCh,
		untrustedHosts:  untrustedHosts,
		mutex:           &sync.Mutex{},
	}
	if trustAllHosts {
		close(c.trustAllHostsCh)
	}
	return c, nil
}

// Wrap the host key callback.
func (c KnownHostsCallback) Wrap() ssh.HostKeyCallback {
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
		// Make sure that interactive user callback does not interfere with
		// SSH connection throttler.
		<-c.throttleCh
		defer func() { c.throttleCh <- struct{}{} }()

		unknown := unknownHost{
			server:     server,
			remote:     remote,
			key:        key,
			hostLine:   knownhosts.Line([]string{server}, key),
			ipLine:     knownhosts.Line([]string{remote.String()}, key),
			responseCh: make(chan response),
		}
		dlog.Common.Warn("Encountered unknown host", unknown)
		// Notify user that there is an unknown host
		c.unknownCh <- unknown
		// Wait for user input.
		switch <-unknown.responseCh {
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
func (c KnownHostsCallback) PromptAddHosts(ctx context.Context) {
	var hosts []unknownHost
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
		case <-time.After(2 * time.Second):
			// Or ask when after 2 seconds no new unknown hosts were added.
			if len(hosts) > 0 {
				c.promptAddHosts(hosts)
				hosts = []unknownHost{}
			}
		case <-ctx.Done():
			dlog.Common.Debug("Stopping goroutine prompting new hosts...")
			return
		}
	}
}

func (c KnownHostsCallback) promptAddHosts(hosts []unknownHost) {
	var servers []string
	for _, host := range hosts {
		servers = append(servers, host.server)
	}

	select {
	case <-c.trustAllHostsCh:
		dlog.Common.Warn("Trusting host keys of servers", servers)
		c.trustHosts(hosts)
		return
	default:
	}

	question := fmt.Sprintf("Encountered %d unknown hosts: '%s'\n%s",
		len(servers),
		strings.Join(servers, ","),
		"Do you want to trust these hosts?",
	)
	p := prompt.New(question)

	a := prompt.Answer{
		Long:  "yes",
		Short: "y",
		Callback: func() {
			c.trustHosts(hosts)
		},
		EndCallback: func() {
			dlog.Common.Info("Added hosts to known hosts file", c.knownHostsPath)
		},
	}
	p.Add(a)

	a = prompt.Answer{
		Long:  "all",
		Short: "a",
		Callback: func() {
			close(c.trustAllHostsCh)
			c.trustHosts(hosts)
		},
		EndCallback: func() {
			dlog.Common.Info("Added hosts to known hosts file", c.knownHostsPath)
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
			dlog.Common.Info("Didn't add hosts to known hosts file", c.knownHostsPath)
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

	p.Ask()
}

func (c KnownHostsCallback) trustHosts(hosts []unknownHost) {
	tmpKnownHostsPath := fmt.Sprintf("%s.tmp", c.knownHostsPath)
	newFd, err := os.OpenFile(tmpKnownHostsPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		panic(fmt.Sprintf("%s: %s", tmpKnownHostsPath, err.Error()))
	}
	defer newFd.Close()

	// Newly trusted hosts in normalized form
	addresses := make(map[string]struct{})
	// First write to new known hosts file, and keep track of addresses
	for _, unknown := range hosts {
		unknown.responseCh <- trustHost

		// Add once as [HOSTNAME]:PORT
		addresses[knownhosts.Normalize(unknown.server)] = struct{}{}
		// And once as [IP]:PORT
		addresses[knownhosts.Normalize(unknown.remote.String())] = struct{}{}

		newFd.WriteString(fmt.Sprintf("%s\n", unknown.hostLine))
		newFd.WriteString(fmt.Sprintf("%s\n", unknown.ipLine))
	}

	// Read old known hosts file, to see which are old and new entries
	os.OpenFile(c.knownHostsPath, os.O_RDONLY|os.O_CREATE, 0666)
	oldFd, err := os.Open(c.knownHostsPath)
	if err != nil {
		panic(err)
	}
	defer oldFd.Close()

	scanner := bufio.NewScanner(oldFd)
	// Now, append all still valid old entries to the new host file
	for scanner.Scan() {
		line := scanner.Text()
		address := strings.SplitN(line, " ", 2)[0]

		if _, ok := addresses[address]; !ok {
			newFd.WriteString(fmt.Sprintf("%s\n", line))
		}
	}

	// Now, replace old known hosts file
	if err := os.Rename(tmpKnownHostsPath, c.knownHostsPath); err != nil {
		panic(err)
	}
}

func (c KnownHostsCallback) dontTrustHosts(hosts []unknownHost) {
	for _, unknown := range hosts {
		unknown.responseCh <- dontTrustHost
	}
}

// Untrusted returns true if the host is not trusted. False otherwise.
func (c KnownHostsCallback) Untrusted(server string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	_, ok := c.untrustedHosts[server]
	return ok
}
