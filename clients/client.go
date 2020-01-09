package clients

import "sync"

// Client is the interface for the end user command line client.
type Client interface {
	Start(wg *sync.WaitGroup) int
	Stop()
}
