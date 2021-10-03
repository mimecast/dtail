package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	// The SSH port number
	SSHPort int
	// Enable experimental features (mainly for dev purposes)
	ExperimentalFeaturesEnable bool `json:",omitempty"`
	// LogLevel defines how much is logged.
	LogLevel string `json:",omitempty"`
	// The log strategy to use, one of
	//   stdout: only log to stdout (useful when used with systemd)
	//   daily: create a log file for every day
	LogStrategy string
	// The log directory
	LogDir string
	// The cache directory
	CacheDir string
	// The temp directory
	TmpDir string `json:",omitempty"`
}

// Create a new default configuration.
func newDefaultCommonConfig() *CommonConfig {
	return &CommonConfig{
		SSHPort:                    DefaultSSHPort,
		ExperimentalFeaturesEnable: false,
		LogDir:                     "log",
		LogLevel:                   "INFO",
		CacheDir:                   "cache",
		TmpDir:                     "/tmp",
	}
}
