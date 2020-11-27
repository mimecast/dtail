package clients

import "context"

// Client is the interface for the end user command line client.
type Client interface {
	Start(ctx context.Context, statsCh <-chan string) int
}
