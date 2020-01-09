package config

// ClientConfig represents a DTail client configuration (empty as of now as there
// are no available config options yet, but that may changes in the future).
type ClientConfig struct {
}

// Create a new default client configuration.
func newDefaultClientConfig() *ClientConfig {
	return &ClientConfig{}
}
