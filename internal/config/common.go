package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	// The SSH port number
	SSHPort int
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
		ExperimentalFeaturesEnable: false,
		LogDir:                     "log",
		Logger:                     "stdout",
		LogLevel:                   DefaultLogLevel,
		LogRotation:                "daily",
		CacheDir:                   "cache",
	}
}
