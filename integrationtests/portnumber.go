package integrationtests

import "sync"

var portNumberMutex sync.Mutex
var currentPortNumber int = 4241

// Go tests can run concurrently, so we need unique TCP port numbers for
// each test.
func getUniquePortNumber() int {
	portNumberMutex.Lock()
	defer portNumberMutex.Unlock()
	currentPortNumber++
	return currentPortNumber
}
