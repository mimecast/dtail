package clients

// Client is the interface for the end user command line client.
type Client interface {
	Start() int
	Stop()
}
