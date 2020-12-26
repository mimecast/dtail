package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	// The SSH port number
	SSHPort int
	// Enable experimental features (mainly for dev purposes)
	ExperimentalFeaturesEnable bool `json:",omitempty"`
	// Enable debug logging. Don't enable in production.
	DebugEnable bool `json:",omitempty"`
	// Enable trace logging. Don't enable in production.
	TraceEnable bool `json:",omitempty"`
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
		SSHPort:                    2222,
		DebugEnable:                false,
		TraceEnable:                false,
		ExperimentalFeaturesEnable: false,
		LogDir:                     "log",
		CacheDir:                   "cache",
		TmpDir:                     "/tmp",
	}
}
