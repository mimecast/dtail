package config

import "os"

// RuntimeConfig contains the active runtime configuration for a process.
// It is intended to be injected into components instead of relying on package globals.
type RuntimeConfig struct {
	Client *ClientConfig
	Server *ServerConfig
	Common *CommonConfig
}

// Hostname returns the configured hostname override or the system hostname.
func (c RuntimeConfig) Hostname() (string, error) {
	if c.Common != nil && c.Common.HostnameOverride != "" {
		return c.Common.HostnameOverride, nil
	}
	return os.Hostname()
}

// CurrentRuntime returns the currently initialized legacy runtime configuration.
//
// Deprecated: inject the RuntimeConfig returned by SetupRuntime.
func CurrentRuntime() RuntimeConfig {
	return RuntimeConfig{
		Client: Client,
		Server: Server,
		Common: Common,
	}
}
