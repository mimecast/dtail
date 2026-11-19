package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	// HostnameOverride replaces the system hostname in logs and output when set.
	HostnameOverride string `json:",omitempty"`
	// The SSH port number
	SSHPort int
	// SSH connection timeout in milliseconds.
	SSHConnectTimeoutMs int `json:",omitempty"`
	// Enable experimental features (mainly for dev purposes)
	ExperimentalFeaturesEnable bool `json:",omitempty"`
	// LogDir defines the log directory.
	LogDir string
	// Logger defines the name of the logger implementation.
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
		ExperimentalFeaturesEnable: false,
		LogDir:                     "log",
		Logger:                     "stdout",
		LogLevel:                   DefaultLogLevel,
		LogRotation:                "daily",
		CacheDir:                   "cache",
	}
}
