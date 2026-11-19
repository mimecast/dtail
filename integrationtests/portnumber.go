package integrationtests

import (
	"os"
	"strconv"
	"sync"
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
func getUniquePortNumber() int {
	portNumberMutex.Lock()
	defer portNumberMutex.Unlock()
	currentPortNumber++
	return currentPortNumber
}
