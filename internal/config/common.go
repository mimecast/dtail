package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	// The SSH server port number.
	SSHPort int
	// Enable experimental features.
	ExperimentalFeaturesEnable bool `json:",omitempty"`
	// Enable extra debug logging (used for deevlopment or debugging purpes only).
	DebugEnable bool `json:",omitempty"`
	// Enable extra trace logging (used for deevlopment or debugging purpes only).
	TraceEnable bool `json:",omitempty"`
	// The log strategy to use, one of
	//   stdout: only log to stdout (useful when used with systemd)
	//   daily: create a log file for every day
	LogStrategy string
	// The log directory
	LogDir string
	// The cache directory
	CacheDir string
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
	}
}
