package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	// HostnameOverride replaces the system hostname in logs and output when set.
	HostnameOverride string `json:",omitempty"`
	// The SSH port number
	SSHPort int
	// SSHConnectTimeoutMs bounds a client's TCP connect to a server, in
	// milliseconds (0 or less: 2000).
	SSHConnectTimeoutMs int `json:",omitempty"`
	// SSHHandshakeTimeoutMs bounds a client's SSH handshake (version and key
	// exchange, authentication) after the TCP connect, in milliseconds (0 or
	// less: 30000). Time spent in the host key check, e.g. waiting for the
	// user's answer to the unknown host question, does not count.
	SSHHandshakeTimeoutMs int `json:",omitempty"`
	// Enable experimental features (mainly for dev purposes)
	ExperimentalFeaturesEnable bool `json:",omitempty"`
	// LogDir defines the log directory. Empty selects the per-command default
	// (DefaultClientLogDir or DefaultServerLogDir); --logDir overrides it.
	// dtailhealth ignores this value and always uses DefaultServerLogDir.
	LogDir string
	// Logger defines the name of the logger implementation. Empty selects the
	// per-command default (DefaultClientLogger, DefaultServerLogger or
	// DefaultHealthCheckLogger); --logger overrides it. dtailhealth ignores
	// this value, so only its --logger flag replaces DefaultHealthCheckLogger.
	Logger string
	// LogLevel defines how much is logged.
	LogLevel string `json:",omitempty"`
	// LogRotation strategy to be used.
	LogRotation string
	// The cache directory
	CacheDir string
}

// Create a new default configuration.
func newDefaultCommonConfig() *CommonConfig {
	return &CommonConfig{
		SSHPort:                    DefaultSSHPort,
		SSHConnectTimeoutMs:        2000,
		SSHHandshakeTimeoutMs:      30000,
		ExperimentalFeaturesEnable: false,
		LogLevel:                   DefaultLogLevel,
		LogRotation:                "daily",
		CacheDir:                   "cache",
	}
}
