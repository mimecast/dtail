package integrationtests

import (
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
)

// defaultPortBase is the port the numbering starts after, unless
// DTAIL_INTEGRATION_TEST_PORT_BASE sets another one. Separate runs of the
// suite on one machine, e.g. from different git worktrees, need different
// bases so their servers do not collide.
const defaultPortBase = 4241

var (
	portNumberMutex   sync.Mutex
	currentPortNumber = portBase()
)

func portBase() int {
	if base, err := strconv.Atoi(os.Getenv("DTAIL_INTEGRATION_TEST_PORT_BASE")); err == nil &&
		base > 0 && base < 65000 {
		return base
	}
	return defaultPortBase
}

// Go tests can run concurrently, so we need unique TCP port numbers for
// each test.
//
// A port another process of the machine already listens on (e.g. a system
// service that happens to use a port of the test range) is skipped: a
// dserver started on it could not bind, and its test failed although nothing
// was wrong with dtail.
func getUniquePortNumber() int {
	portNumberMutex.Lock()
	defer portNumberMutex.Unlock()
	for {
		currentPortNumber++
		if portIsFree(currentPortNumber) {
			return currentPortNumber
		}
	}
}

// portIsFree reports whether the port can be bound on the addresses the
// tests bind their servers to (localhost resolves to both loopback
// addresses; a host without IPv6 only needs the IPv4 one).
func portIsFree(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			if host == "::1" && !errors.Is(err, syscall.EADDRINUSE) {
				continue
			}
			return false
		}
		_ = listener.Close()
	}
	return true
}
